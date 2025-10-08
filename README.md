# WebSocket Chat Server

## Установка Docker на Linux

### Ubuntu/Debian:
```bash
# Обновляем пакеты
sudo apt update

# Устанавливаем зависимости
sudo apt install apt-transport-https ca-certificates curl gnupg lsb-release

# Добавляем GPG ключ Docker
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg

# Добавляем репозиторий Docker
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

# Устанавливаем Docker
sudo apt update
sudo apt install docker-ce docker-ce-cli containerd.io

# Устанавливаем Docker Compose
sudo curl -L "https://github.com/docker/compose/releases/latest/download/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
sudo chmod +x /usr/local/bin/docker-compose

# Добавляем пользователя в группу docker
sudo usermod -aG docker $USER
```



## Запуск сервера

1. Клонируйте репозиторий:
   ```bash
   git clone <repository-url>
   cd websocket
   ```

2. Запустите сервер:
   ```bash
   docker-compose up --build
   ```

3. Сервер будет доступен по адресу `ws://localhost:12345/ws`

Файл `.env` уже включен в репозиторий с настройками по умолчанию.
