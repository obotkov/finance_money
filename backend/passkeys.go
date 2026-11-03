package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Вход по passkey (WebAuthn). Passkey добавляют в Настройках, уже войдя (через
// Google или другим passkey), а дальше входят без почты: браузер сам предлагает
// ключи этого сайта (discoverable credentials). Аккаунт через passkey не создаётся —
// только через Google. Между началом и концом церемонии вызов (challenge) живёт
// в памяти сервера под случайным токеном из куки.

const (
	passkeyCookie   = "passkey_flow"
	passkeyFlowTTL  = 5 * time.Minute
	maxPasskeyName  = 60
	maxPasskeysUser = 20
	// начатых и не законченных церемоний одновременно: начать вход может кто угодно,
	// и без предела ими можно забить память
	maxPasskeyFlows = 5000
)

// newWebAuthn настраивает проверяющую сторону по адресу сайта: RP ID — его домен,
// принимается только его origin. Без PUBLIC_URL (тесты) passkeys выключены.
func newWebAuthn(publicURL string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("PUBLIC_URL %q: нужен адрес сайта", publicURL)
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Penny",
		RPOrigins:     []string{strings.TrimRight(publicURL, "/")},
	})
}

// passkeyFlows — начатые церемонии: токен из куки → вызов и чей он (0 — вход).
type passkeyFlows struct {
	mu sync.Mutex
	m  map[string]passkeyFlow
}

type passkeyFlow struct {
	data    webauthn.SessionData
	uid     int64
	expires time.Time
}

// put запоминает церемонию и отдаёт токен для куки; "" — их слишком много.
func (f *passkeyFlows) put(data webauthn.SessionData, uid int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if f.m == nil {
		f.m = map[string]passkeyFlow{}
	}
	for k, v := range f.m {
		if now.After(v.expires) {
			delete(f.m, k)
		}
	}
	if len(f.m) >= maxPasskeyFlows {
		return ""
	}
	token := randomToken()
	f.m[token] = passkeyFlow{data: data, uid: uid, expires: now.Add(passkeyFlowTTL)}
	return token
}

// take отдаёт церемонию один раз: повторить ответ на тот же вызов нельзя.
func (f *passkeyFlows) take(r *http.Request) (passkeyFlow, bool) {
	c, err := r.Cookie(passkeyCookie)
	if err != nil {
		return passkeyFlow{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	flow, ok := f.m[c.Value]
	delete(f.m, c.Value)
	return flow, ok && time.Now().Before(flow.expires)
}

// passkeyUser — пользователь глазами go-webauthn. Идентификатор (user handle) —
// id пользователя строкой: по нему вход находит аккаунт.
type passkeyUser struct {
	user  User
	creds []webauthn.Credential
}

func (p passkeyUser) WebAuthnID() []byte                         { return userHandle(p.user.ID) }
func (p passkeyUser) WebAuthnName() string                       { return p.user.Email }
func (p passkeyUser) WebAuthnDisplayName() string                { return cmp.Or(p.user.Name, p.user.Email) }
func (p passkeyUser) WebAuthnCredentials() []webauthn.Credential { return p.creds }

func userHandle(id int64) []byte { return []byte(strconv.FormatInt(id, 10)) }

// PasskeyInfo — строка списка в Настройках.
type PasskeyInfo struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (s *Store) Passkeys(ctx context.Context, uid int64) ([]PasskeyInfo, error) {
	rows, _ := s.db.Query(ctx, `SELECT id, name, created_at, last_used_at FROM passkeys WHERE user_id = $1 ORDER BY id`, uid)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PasskeyInfo, error) {
		var p PasskeyInfo
		err := r.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt)
		return p, err
	})
}

func (s *Store) passkeyUser(ctx context.Context, uid int64) (*passkeyUser, error) {
	p := &passkeyUser{}
	err := s.db.QueryRow(ctx, `SELECT id, email, name FROM users WHERE id = $1`, uid).Scan(&p.user.ID, &p.user.Email, &p.user.Name)
	if err != nil {
		return nil, err
	}
	rows, _ := s.db.Query(ctx, `SELECT data FROM passkeys WHERE user_id = $1 ORDER BY id`, uid)
	p.creds, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (webauthn.Credential, error) {
		var c webauthn.Credential
		err := r.Scan(&c)
		return c, err
	})
	return p, err
}

func (s *Store) addPasskey(ctx context.Context, uid int64, name string, c *webauthn.Credential) error {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM passkeys WHERE user_id = $1`, uid).Scan(&n); err != nil {
		return err
	}
	if n >= maxPasskeysUser {
		return badRequest(fmt.Sprintf("Passkeys не больше %d — удалите ненужный", maxPasskeysUser))
	}
	_, err := s.db.Exec(ctx, `INSERT INTO passkeys (user_id, credential_id, data, name) VALUES ($1, $2, $3, $4)`, uid, c.ID, c, name)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return badRequest("Этот passkey уже добавлен")
	}
	return err
}

// passkeyUsed сохраняет ключ после входа: счётчик подписей и флаги могли измениться.
func (s *Store) passkeyUsed(ctx context.Context, c *webauthn.Credential) error {
	_, err := s.db.Exec(ctx, `UPDATE passkeys SET data = $2, last_used_at = now() WHERE credential_id = $1`, c.ID, c)
	return err
}

// RenamePasskey — название, по которому ключ узнают в списке.
func (s *Store) RenamePasskey(ctx context.Context, uid, id int64, in struct {
	Name string `json:"name"`
}) error {
	name, err := passkeyName(in.Name)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE passkeys SET name = $3 WHERE id = $1 AND user_id = $2`, id, uid, name)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Passkey не найден")
	}
	return err
}

func (s *Store) DeletePasskey(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM passkeys WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Passkey не найден")
	}
	return err
}

func passkeyName(s string) (string, error) {
	name := strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(name) > maxPasskeyName {
		return "", badRequest(fmt.Sprintf("Название длиннее %d символов", maxPasskeyName))
	}
	if name == "" {
		name = "Passkey"
	}
	return name, nil
}

// startPasskeyFlow кладёт токен церемонии в куку; false — ответ уже отправлен.
func (a *API) startPasskeyFlow(w http.ResponseWriter, data webauthn.SessionData, uid int64) bool {
	token := a.passkeys.put(data, uid)
	if token == "" {
		writeJSON(w, http.StatusTooManyRequests, errorBody("Слишком много попыток входа — попробуйте через несколько минут"))
		return false
	}
	a.setCookie(w, passkeyCookie, token, "/api", passkeyFlowTTL)
	return true
}

// passkeyLoginStart — вызов для входа: без списка ключей, браузер предложит свои.
func (a *API) passkeyLoginStart(w http.ResponseWriter, r *http.Request) {
	if a.webauthn == nil {
		http.NotFound(w, r)
		return
	}
	assertion, data, err := a.webauthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !a.startPasskeyFlow(w, *data, 0) {
		return
	}
	writeJSON(w, http.StatusOK, assertion)
}

// passkeyLoginFinish проверяет подпись ключа и открывает сессию его владельца.
func (a *API) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if a.webauthn == nil {
		http.NotFound(w, r)
		return
	}
	flow, ok := a.passkeys.take(r)
	a.setCookie(w, passkeyCookie, "", "/api", -1)
	if !ok || flow.uid != 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("Вход по passkey устарел — нажмите кнопку ещё раз"))
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("Браузер прислал непонятный ответ passkey"))
		return
	}
	var owner *passkeyUser
	find := func(rawID, handle []byte) (webauthn.User, error) {
		uid, err := strconv.ParseInt(string(handle), 10, 64)
		if err != nil {
			return nil, err
		}
		if owner, err = a.store.passkeyUser(r.Context(), uid); err != nil {
			return nil, err
		}
		return owner, nil
	}
	cred, err := a.webauthn.ValidateDiscoverableLogin(find, flow.data, parsed)
	if err != nil {
		a.log.Info("passkey login refused", "err", err)
		writeJSON(w, http.StatusUnauthorized, errorBody("Этот passkey не подошёл: возможно, его удалили в Настройках. Войдите через Google и добавьте новый."))
		return
	}
	if err := a.store.passkeyUsed(r.Context(), cred); err != nil {
		a.fail(w, r, err)
		return
	}
	u := owner.user
	a.signIn(w, r, &u)
}

// passkeyAddStart — вызов для нового ключа вошедшего пользователя. Уже добавленные
// ключи исключаются: тот же менеджер паролей не создаст второй.
func (a *API) passkeyAddStart(w http.ResponseWriter, r *http.Request) {
	if a.webauthn == nil {
		http.NotFound(w, r)
		return
	}
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	pu, err := a.store.passkeyUser(r.Context(), u.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// ключ должен проверять человека (отпечаток, лицо, PIN): вход этого требует
	creation, data, err := a.webauthn.BeginRegistration(pu,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(), // то же для браузеров первой версии WebAuthn
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(webauthn.Credentials(pu.creds).CredentialDescriptors()),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps()))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !a.startPasskeyFlow(w, *data, u.ID) {
		return
	}
	writeJSON(w, http.StatusOK, creation)
}

// AddPasskey проверяет ответ браузера на вызов из passkeyAddStart и сохраняет ключ.
func (a *API) addPasskey(r *http.Request, uid int64) error {
	var in struct {
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	name, err := passkeyName(in.Name)
	if err != nil {
		return err
	}
	flow, ok := a.passkeys.take(r)
	if !ok || flow.uid != uid {
		return badRequest("Добавление passkey устарело — нажмите кнопку ещё раз")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(in.Credential)
	if err != nil {
		return badRequest("Браузер прислал непонятный ответ passkey")
	}
	pu, err := a.store.passkeyUser(r.Context(), uid)
	if err != nil {
		return err
	}
	cred, err := a.webauthn.CreateCredential(pu, flow.data, parsed)
	if err != nil {
		a.log.Info("passkey registration refused", "err", err)
		return badRequest("Passkey не прошёл проверку — попробуйте ещё раз")
	}
	return a.store.addPasskey(r.Context(), uid, name, cred)
}
