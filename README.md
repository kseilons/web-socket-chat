# WebSocket Chat Server

WebSocket чат-сервер на Go с поддержкой Docker и переменных окружения.

## Запуск через Docker Compose

1. Скопируйте файл `env.example` в `.env`:
   ```bash
   cp env.example .env
   ```

2. При необходимости отредактируйте `.env` файл:
   ```
   SERVER_HOST=0.0.0.0
   SERVER_PORT=12345
   ```

3. Запустите сервер:
   ```bash
   docker-compose up --build
   ```

4. Сервер будет доступен по адресу `ws://localhost:12345/ws`

## Обычный запуск

1. Убедитесь, что у вас установлен Go 1.21 или выше

2. Установите зависимости:
   ```bash
   go mod download
   ```

3. Запустите сервер:
   ```bash
   go run server/server.go
   ```

4. Следуйте инструкциям на экране для ввода хоста и порта

## Использование переменных окружения

Вы можете задать настройки сервера через переменные окружения:

- `SERVER_HOST` - адрес сервера (по умолчанию: 0.0.0.0)
- `SERVER_PORT` - порт сервера (по умолчанию: 12345)

Пример:
```bash
export SERVER_HOST=127.0.0.1
export SERVER_PORT=8080
go run server/server.go
```

## Команды Docker

- Сборка образа: `docker-compose build`
- Запуск: `docker-compose up`
- Запуск в фоне: `docker-compose up -d`
- Остановка: `docker-compose down`
- Просмотр логов: `docker-compose logs -f`

## Функции чата

- Публичные сообщения
- Личные сообщения (@ник сообщение)
- Массовые личные сообщения (#all сообщение)
- Список пользователей (#users)
- Блокировка пользователей (#block ник, #unblock ник)
- Любимые писатели (#fav add ник, #fav remove ник)
- Почтовый ящик для оффлайн сообщений (#mailbox)
- История никнеймов по IP
