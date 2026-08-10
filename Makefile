.PHONY: help demo-up demo-down demo-logs up down logs image \
        build test fmt vendor-viewer

help:
	@echo "Демо целиком в докере (ничего настраивать не нужно):"
	@echo "  make demo-up       Spectra + Postgres + семь фейковых сервисов"
	@echo "                     → http://localhost:8080  (admin / admin)"
	@echo "  make demo-down     остановить и удалить данные"
	@echo "  make demo-logs     смотреть логи"
	@echo ""
	@echo "Своя установка (настройка — через переменные окружения, см. README):"
	@echo "  make up            поднять Spectra и Postgres"
	@echo "  make down          остановить"
	@echo "  make image         собрать образ spectra:latest"
	@echo ""
	@echo "Kubernetes: см. deploy/DEPLOY.md"
	@echo ""
	@echo "Разработка (нужен Go):"
	@echo "  make test / fmt / build"

# --- демо-стенд -----------------------------------------------------------

demo-up:
	docker compose -f demo/docker-compose.yml up --build -d
	@echo "Spectra: http://localhost:8080  (admin / admin)"

demo-down:
	docker compose -f demo/docker-compose.yml down -v

demo-logs:
	docker compose -f demo/docker-compose.yml logs -f

# --- своя установка -------------------------------------------------------

up:
	docker compose up --build -d
	@echo "Spectra: http://localhost:8080"

down:
	docker compose down

logs:
	docker compose logs -f

image:
	docker build -t spectra:latest .

# --- разработка -----------------------------------------------------------

# Половина логики store живёт в SQL, поэтому тестам нужен настоящий Postgres.
# Он поднимается одноразовым контейнером и гасится после прогона.
test:
	@docker rm -f spectra-test-pg >/dev/null 2>&1 || true
	@docker run -d --name spectra-test-pg \
		-e POSTGRES_USER=spectra -e POSTGRES_PASSWORD=spectra -e POSTGRES_DB=spectra_test \
		-p 55432:5432 postgres:17-alpine >/dev/null
	@printf "жду Postgres"; \
	for i in $$(seq 1 40); do \
		docker exec spectra-test-pg pg_isready -U spectra -d spectra_test 2>/dev/null | grep -q accepting && break; \
		printf "."; sleep 1; \
	done; echo
	-@SPECTRA_TEST_DATABASE_URL='postgres://spectra:spectra@localhost:55432/spectra_test?sslmode=disable' \
		go test ./...
	@docker rm -f spectra-test-pg >/dev/null 2>&1 || true

# Тесты без Postgres: те, которым он нужен, пропускаются.
test-quick:
	go test ./...

fmt:
	gofmt -w .

build:
	go build -o bin/spectra ./cmd/spectra

# Обновить встроенный вьюер спецификаций.
vendor-viewer:
	curl -sSL -o internal/web/static/vendor/scalar.standalone.js \
		https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.130/dist/browser/standalone.js
