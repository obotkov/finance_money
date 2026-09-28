package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Бюджет — месячный лимит трат на категорию в рублях. Траты считаются по
// категории вместе с подкатегориями, только расходы, за календарный месяц по
// Москве; суммы не в рублях переводятся по текущему курсу, как на странице.

type Budget struct {
	ID       int64   `json:"id"`
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
}

type BudgetInput struct {
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
}

// BudgetAlert — когда слать уведомление: при Pct процентах лимита, и отдельно
// при превышении.
type BudgetAlert struct {
	On  bool `json:"on"`
	Pct int  `json:"pct"`
}

func (s *Store) budgets(ctx context.Context, uid int64) ([]Budget, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT b.id, c.name, b.amount FROM budgets b JOIN categories c ON c.id = b.category_id
		WHERE b.user_id = $1 ORDER BY c.name`, uid)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Budget, error) {
		var b Budget
		err := r.Scan(&b.ID, &b.Category, &b.Amount)
		return b, err
	})
}

func (s *Store) budgetAlert(ctx context.Context, uid int64) (BudgetAlert, error) {
	var a BudgetAlert
	err := s.db.QueryRow(ctx, `SELECT budget_alert_on, budget_alert_pct FROM users WHERE id = $1`, uid).Scan(&a.On, &a.Pct)
	return a, err
}

// expenseCategory находит категорию расходов пользователя по названию.
func expenseCategory(ctx context.Context, q pgx.Tx, uid int64, name string) (int64, error) {
	var id int64
	var kind string
	err := q.QueryRow(ctx, `SELECT id, kind FROM categories WHERE user_id = $1 AND name = $2`, uid, strings.TrimSpace(name)).Scan(&id, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, badRequest("Категория «" + name + "» не найдена")
	}
	if err != nil {
		return 0, err
	}
	if kind != "expense" {
		return 0, badRequest("Бюджет ставится на категорию расходов")
	}
	return id, nil
}

func (in *BudgetInput) validate() error {
	if strings.TrimSpace(in.Category) == "" {
		return badRequest("Выберите категорию")
	}
	if !(in.Amount > 0) {
		return badRequest("Лимит должен быть больше нуля")
	}
	return nil
}

func (s *Store) CreateBudget(ctx context.Context, uid int64, in BudgetInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cat, err := expenseCategory(ctx, tx, uid, in.Category)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO budgets (user_id, category_id, amount) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, category_id) DO NOTHING`, uid, cat, in.Amount)
		if err == nil && tag.RowsAffected() == 0 {
			return badRequest("На «" + in.Category + "» бюджет уже есть — измените его лимит")
		}
		return err
	})
}

// UpdateBudget меняет категорию или лимит. Отметки об уведомлениях этого
// месяца сбрасываются: с новым лимитом порог считается заново.
func (s *Store) UpdateBudget(ctx context.Context, uid, id int64, in BudgetInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cat, err := expenseCategory(ctx, tx, uid, in.Category)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE budgets SET category_id = $3, amount = $4 WHERE id = $1 AND user_id = $2`, id, uid, cat, in.Amount)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return notFound("Бюджет не найден")
		}
		_, err = tx.Exec(ctx, `DELETE FROM budget_alerts WHERE budget_id = $1 AND month = $2`, id, moscowMonth())
		return err
	})
}

func (s *Store) DeleteBudget(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM budgets WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Бюджет не найден")
	}
	return err
}

// SetBudgetAlert включает или выключает уведомления и задаёт порог в процентах.
func (s *Store) SetBudgetAlert(ctx context.Context, uid int64, in BudgetAlert) error {
	if in.Pct < 10 || in.Pct > 100 {
		return badRequest("Порог — от 10 до 100 %")
	}
	_, err := s.db.Exec(ctx, `UPDATE users SET budget_alert_on = $2, budget_alert_pct = $3 WHERE id = $1`, uid, in.On, in.Pct)
	return err
}

func moscowMonth() string { return time.Now().In(moscow).Format("2006-01") }

// budgetUsage — бюджет с тратами за месяц, в рублях.
type budgetUsage struct {
	ID       int64
	Category string
	Limit    float64
	Spent    float64
}

// budgetUsage считает траты по каждому бюджету за месяц month (ГГГГ-ММ).
func (s *Store) budgetUsage(ctx context.Context, uid int64, month string) ([]budgetUsage, error) {
	from, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, err
	}
	rows, _ := s.db.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT b.id AS budget_id, b.category_id AS cat_id FROM budgets b WHERE b.user_id = $1
			UNION ALL
			SELECT t.budget_id, c.id FROM categories c JOIN tree t ON c.parent_id = t.cat_id WHERE c.user_id = $1
		), spent AS (
			SELECT tree.budget_id, SUM(x.amount * COALESCE(r.rub, 1)) AS rub
			FROM tree
			JOIN transactions x ON x.category_id = tree.cat_id AND x.user_id = $1 AND x.type = 'expense'
			                   AND x.date >= $2 AND x.date < $3
			JOIN accounts a ON a.id = x.account_id
			LEFT JOIN rates r ON r.code = a.currency AND a.currency <> 'RUB'
			GROUP BY tree.budget_id
		)
		SELECT b.id, c.name, b.amount, COALESCE(s.rub, 0)
		FROM budgets b JOIN categories c ON c.id = b.category_id LEFT JOIN spent s ON s.budget_id = b.id
		WHERE b.user_id = $1 ORDER BY b.id`, uid, from, from.AddDate(0, 1, 0))
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (budgetUsage, error) {
		var u budgetUsage
		err := r.Scan(&u.ID, &u.Category, &u.Limit, &u.Spent)
		return u, err
	})
}

// budgetLevel — какое уведомление положено: over — лимит превышен, warn —
// траты дошли до порога, "" — ничего.
func budgetLevel(spent, limit float64, pct int) string {
	switch {
	case limit <= 0:
		return ""
	case spent > limit:
		return "over"
	case spent >= limit*float64(pct)/100:
		return "warn"
	}
	return ""
}

// dueBudgetAlerts возвращает бюджеты, по которым пора уведомить, и сразу
// отмечает уведомления отправленными — каждое уходит раз за месяц. После
// превышения отметка порога тоже ставится, чтобы порог не пришёл следом.
func (s *Store) dueBudgetAlerts(ctx context.Context, uid int64) ([]budgetUsage, []string, error) {
	alert, err := s.budgetAlert(ctx, uid)
	if err != nil || !alert.On {
		return nil, nil, err
	}
	month := moscowMonth()
	usage, err := s.budgetUsage(ctx, uid, month)
	if err != nil {
		return nil, nil, err
	}
	var due []budgetUsage
	var levels []string
	for _, u := range usage {
		level := budgetLevel(u.Spent, u.Limit, alert.Pct)
		if level == "" {
			continue
		}
		marks := []string{level}
		if level == "over" {
			marks = append(marks, "warn")
		}
		var fresh bool
		for i, m := range marks {
			tag, err := s.db.Exec(ctx, `
				INSERT INTO budget_alerts (budget_id, month, level) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, u.ID, month, m)
			if err != nil {
				return nil, nil, err
			}
			if i == 0 {
				fresh = tag.RowsAffected() > 0
			}
		}
		if fresh {
			due, levels = append(due, u), append(levels, level)
		}
	}
	return due, levels, nil
}
