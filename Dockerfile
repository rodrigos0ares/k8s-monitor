FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o k8s-monitor ./cmd/

FROM alpine:3.20
RUN apk add --no-cache sqlite-libs ca-certificates
COPY --from=builder /app/k8s-monitor /usr/local/bin/

ENTRYPOINT ["k8s-monitor"]
