FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# на сервере 1 ГБ памяти: GOGC=25 чаще чистит память компилятора — пик сборки
# (pgx/pgtype) падает примерно с 390 до 300 МБ; сборка чуть дольше
RUN CGO_ENABLED=0 GOGC=25 go build -trimpath -ldflags="-s -w" -o /out/api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
