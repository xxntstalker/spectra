# Демо-стенд: фейковые сервисы, публикующие спеки разными способами.
# Собирается из корня репозитория, потому что нужен go.mod:
#   docker build -f demo/Dockerfile -t spectra-demo .
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY demo ./demo

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/demoservices ./demo


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/demoservices /demoservices
EXPOSE 7101 7102 7103 7104 7105 7106 7107
USER nonroot:nonroot
ENTRYPOINT ["/demoservices"]
