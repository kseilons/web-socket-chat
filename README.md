# WebSocket Chat Server

## Установка Docker на Linux

### Ubuntu/Debian:
```bash
# Обновляем пакеты
sudo apt update
sudo apt install curl software-properties-common ca-certificates apt-transport-https -y
wget -O- https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor | sudo tee /etc/apt/keyrings/docker.gpg > /dev/null
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu jammy stable"| sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

sudo apt update

sudo apt install -y docker-ce docker-ce-cli containerd.io

sudo systemctl enable docker
sudo systemctl start docker

sudo curl -L "https://github.com/docker/compose/releases/latest/download/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
sudo chmod +x /usr/local/bin/docker-compose

sudo ln -s /usr/local/bin/docker-compose /usr/bin/docker-compose
```



# Настраиваем зеркала для Docker Hub
```
nano /etc/docker/daemon.json
```
```
{
  "registry-mirrors": [
    "https://dockerhub.timeweb.cloud",
    "https://dockerhub1.beget.com",
    "https://mirror.gcr.io",
    "https://ghcr.io",
    "https://quay.io",
    "https://public.ecr.aws"
  ]
}
```
```
# Перезапускаем Docker
sudo systemctl restart docker
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
пью горький кофе? дрищу в торговом центре

