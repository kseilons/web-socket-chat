# Используем официальный образ Go
FROM golang:1.25-alpine AS builder

# Устанавливаем рабочую директорию
WORKDIR /app

# Копируем go mod и sum файлы
COPY go.mod go.sum ./

# Загружаем зависимости
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем приложение
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o server ./server/server.go

# Финальный образ
FROM alpine:latest

# Устанавливаем ca-certificates для HTTPS запросов
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Копируем собранное приложение
COPY --from=builder /app/server .

# Открываем порт
EXPOSE 12345

# Запускаем приложение
CMD ["./server"]
