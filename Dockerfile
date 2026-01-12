FROM golang:1.25.0-alpine3.21 AS builder

WORKDIR /app

COPY . /app

RUN apk add --no-cache gcc musl-dev

RUN go mod download

RUN CGO_ENABLED=1 go build -o whatsapp-worker

FROM alpine:latest

WORKDIR /root/

RUN mkdir storage

COPY --from=builder /app/whatsapp-worker .

CMD ["./whatsapp-worker"]
