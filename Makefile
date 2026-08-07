.PHONY: help demo-up demo-down demo-logs up down logs image \
        run build test fmt reset vendor-viewer

help:
	@echo "Демо целиком в докере (ничего настраивать не нужно):"
	@echo "  make demo-up       портал + семь фейковых сервисов → http://localhost:8080"
	@echo "  make demo-down     остановить и удалить данные демо"
	@echo ""
	@echo "Свой реестр:"
	@echo "  cp services.example.yaml services.yaml   и вписать свои сервисы"
	@echo "  make up            поднять портал"
	@echo "  make down          остановить"
	@echo "  make image         собрать образ api-portal:latest"
	@echo ""
	@echo "Разработка (нужен Go):"
	@echo "  make test / fmt / run / build"

# --- демо-стенд -----------------------------------------------------------

demo-up:
	docker compose -f demo/docker-compose.yml up --build -d
	@echo "Портал: http://localhost:8080"

demo-down:
	docker compose -f demo/docker-compose.yml down -v

demo-logs:
	docker compose -f demo/docker-compose.yml logs -f

# --- портал на своём реестре ----------------------------------------------

# Проверка не формальная: Docker молча создаёт каталог на месте отсутствующего
# файла для bind-mount, и портал падает с невнятным "is a directory".
up:
	@test -f services.yaml || { \
		echo "Нет services.yaml."; \
		echo "  cp services.example.yaml services.yaml   — и вписать свои сервисы"; \
		echo "  make demo-up                             — посмотреть на демо-данных"; \
		exit 1; }
	docker compose up --build -d
	@echo "Портал: http://localhost:8080"

down:
	docker compose down

logs:
	docker compose logs -f

image:
	docker build -t api-portal:latest .

# --- разработка -----------------------------------------------------------

test:
	go test ./...

fmt:
	gofmt -w .

# Локальный запуск без докера. Демо-сервисы поднимаются отдельно: go run ./demo
run:
	go run ./cmd/portal -config demo/services.yaml

build:
	go build -o bin/portal ./cmd/portal

# --- обслуживание ---------------------------------------------------------

# Забыть историю, собранную локальным запуском (в докере данные в томе,
# их убирает make demo-down / docker compose down -v).
reset:
	rm -rf data

# Обновить встроенный вьюер спецификаций.
vendor-viewer:
	curl -sSL -o internal/web/static/vendor/scalar.standalone.js \
		https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.130/dist/browser/standalone.js
