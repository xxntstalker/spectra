# Образ портала. Демо-стенд сюда не попадает и даже не компилируется —
# он живёт в demo/ со своим Dockerfile.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем — пересобираются только при правке go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/spectra ./cmd/spectra
COPY internal ./internal

# CGO не нужен: SQLite здесь чистый Go (modernc.org/sqlite), поэтому бинарник
# получается статическим и уезжает в distroless без единой библиотеки рядом.
# UI и вьюер спецификаций вшиты в бинарник через go:embed.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/spectra ./cmd/spectra


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/spectra /spectra

# Состояния на диске больше нет: реестр, настройки и история — в Postgres,
# адрес которого приходит через SPECTRA_DATABASE_URL. Контейнер stateless.
EXPOSE 8080
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/spectra", "-health-check"]

ENTRYPOINT ["/spectra"]
