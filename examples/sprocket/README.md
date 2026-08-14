# Sprocket

Sprocket is a standard-library Go REST service example generated from `pattern-go-rest-service`.

## Run

```bash
go run ./cmd/sprocket
```

Set `PORT` to override the default `8080`.

## Endpoints

- `GET /healthz`
- `GET /readyz`
- `POST /sprockets`
- `GET /sprockets`
- `GET /sprockets/{id}`
- `DELETE /sprockets/{id}`

## Verify

```bash
make build
make vet
make test
```
