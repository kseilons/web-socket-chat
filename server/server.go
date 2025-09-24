package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Client struct {
	conn          net.Conn
	nickname      string
	address       string
	writer        *bufio.Writer
	blocked       map[string]bool
	favoriteUsers map[string]bool
}

type MailboxMessage struct {
	From    string
	Message string
	Time    time.Time
}

type Mailbox struct {
	Messages []MailboxMessage
	Mutex    sync.RWMutex
}

type ChatServer struct {
	host         string
	port         int
	listener     net.Listener
	clients      []*Client
	mutex        sync.Mutex
	running      bool
	userHistory  map[string]string
	historyMutex sync.RWMutex
	mailboxes    map[string]*Mailbox // никнейм -> почтовый ящик
	mailboxMutex sync.RWMutex
}

func NewChatServer(host string, port int) *ChatServer {
	return &ChatServer{
		host:        host,
		port:        port,
		clients:     make([]*Client, 0),
		running:     false,
		userHistory: make(map[string]string),
		mailboxes:   make(map[string]*Mailbox),
	}
}

func (s *ChatServer) Start() error {
	address := fmt.Sprintf("%s:%d", s.host, s.port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("не удалось запустить сервер: %v", err)
	}

	s.listener = listener
	s.running = true

	fmt.Printf("🚀 Чат-сервер запущен на %s\n", address)
	fmt.Println("Ожидание подключений...")
	fmt.Println("Личные сообщения: @никнейм сообщение")

	// Обработка сигналов для graceful shutdown
	go s.handleSignals()

	// Основной цикл принятия подключений
	for s.running {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.running {
				fmt.Printf("❌ Ошибка accept: %v\n", err)
			}
			continue
		}

		clientAddr := conn.RemoteAddr().String()
		fmt.Printf("📱 Новое подключение: %s\n", clientAddr)

		// Обрабатываем клиента в отдельной горутине
		go s.handleClient(conn, clientAddr)
	}

	return nil
}

func (s *ChatServer) getOrCreateMailbox(nickname string) *Mailbox {
	s.mailboxMutex.Lock()
	defer s.mailboxMutex.Unlock()

	if mailbox, exists := s.mailboxes[nickname]; exists {
		return mailbox
	}

	mailbox := &Mailbox{
		Messages: make([]MailboxMessage, 0),
	}
	s.mailboxes[nickname] = mailbox
	return mailbox
}

func (s *ChatServer) addOfflineMessage(to, from, message string) bool {
	mailbox := s.getOrCreateMailbox(to)
	mailbox.Mutex.Lock()
	defer mailbox.Mutex.Unlock()

	// Проверяем лимит сообщений (максимум 10)
	if len(mailbox.Messages) >= 10 {
		return false // Ящик переполнен
	}

	mailbox.Messages = append(mailbox.Messages, MailboxMessage{
		From:    from,
		Message: message,
		Time:    time.Now(),
	})
	return true
}

func (s *ChatServer) deliverOfflineMessages(client *Client) {
	mailbox := s.getOrCreateMailbox(client.nickname)
	mailbox.Mutex.Lock()
	defer mailbox.Mutex.Unlock()

	if len(mailbox.Messages) == 0 {
		return
	}

	// Доставляем все сообщения
	for _, msg := range mailbox.Messages {
		timestamp := msg.Time.Format("15:04:05")
		offlineMsg := fmt.Sprintf("[📮][%s] %s (оффлайн): %s", timestamp, msg.From, msg.Message)
		s.sendToClient(client, offlineMsg)
	}

	// Уведомляем пользователя
	s.sendToClient(client, fmt.Sprintf("📬 Вам доставлено %d отложенных сообщений", len(mailbox.Messages)))

	// Очищаем ящик после доставки
	mailbox.Messages = make([]MailboxMessage, 0)

}

func (s *ChatServer) getMailboxStatus(client *Client) {
	mailbox := s.getOrCreateMailbox(client.nickname)
	mailbox.Mutex.RLock()
	defer mailbox.Mutex.RUnlock()

	count := len(mailbox.Messages)
	if count == 0 {
		s.sendToClient(client, "📭 Ваш почтовый ящик пуст")
	} else {
		s.sendToClient(client, fmt.Sprintf("📬 У вас %d отложенных сообщений", count))
	}
}

func (s *ChatServer) getPreviousNickname(ip string) string {
	s.historyMutex.RLock()
	defer s.historyMutex.RUnlock()

	return s.userHistory[ip]
}

func (s *ChatServer) saveNicknameHistory(ip, nickname string) {
	s.historyMutex.Lock()
	defer s.historyMutex.Unlock()

	s.userHistory[ip] = nickname
}

func (s *ChatServer) findClientByNickname(nickname string) *Client {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients {
		if client.nickname == nickname {
			return client
		}
	}
	return nil
}

func (s *ChatServer) handleClient(conn net.Conn, address string) {
	var nickname string
	var client *Client

	defer func() {
		if client != nil {
			s.disconnectClient(client)
		} else {
			conn.Close()
		}
	}()

	// Извлекаем IP из адреса
	ip := strings.Split(address, ":")[0]

	// Проверяем историю для этого IP
	previousNickname := s.getPreviousNickname(ip)

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Если есть предыдущий никнейм, предлагаем его
	if previousNickname != "" {
		prompt := fmt.Sprintf("NICK_PROMPT:%s\n", previousNickname)
		writer.WriteString(prompt)
		writer.Flush()
		fmt.Printf("📝 Предлагаем никнейм '%s' для IP %s\n", previousNickname, ip)
	} else {
		writer.WriteString("NICK_REQUEST\n")
		writer.Flush()
	}

	nickRequest, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("❌ Ошибка чтения никнейма от %s: %v\n", address, err)
		return
	}

	nickRequest = strings.TrimSpace(nickRequest)

	// Обрабатываем ответ с предложенным никнеймом
	if strings.HasPrefix(nickRequest, "NICK:") {
		nickname = strings.TrimPrefix(nickRequest, "NICK:")
		nickname = strings.TrimSpace(nickname)
	} else {
		// Если пользователь просто ввел никнейм (без префикса)
		nickname = strings.TrimSpace(nickRequest)
	}

	if nickname == "" {
		writer.WriteString("ERROR: Nickname cannot be empty\n")
		writer.Flush()
		return
	}

	// Проверяем, не занят ли никнейм
	if s.isNicknameTaken(nickname) {
		writer.WriteString("NICK_TAKEN\n")
		writer.Flush()
		return
	}

	// Сохраняем в историю
	s.saveNicknameHistory(ip, nickname)

	// Создаем клиента с инициализированной картой blocked
	client = &Client{
		conn:          conn,
		nickname:      nickname,
		address:       address,
		writer:        writer,
		blocked:       make(map[string]bool),
		favoriteUsers: make(map[string]bool),
	}

	// Добавляем клиента в список
	s.addClient(client)

	// Отправляем подтверждение ПЕРВЫМ
	writer.WriteString("NICK_OK\n")
	writer.Flush()

	// ТОЛЬКО ПОСЛЕ успешной аутентификации доставляем отложенные сообщения
	s.deliverOfflineMessages(client)

	// Уведомляем всех о новом пользователе
	joinMessage := fmt.Sprintf("🟢 %s присоединился к чату", nickname)

	// Добавляем информацию о повторном входе, если применимо
	if previousNickname != "" && previousNickname == nickname {
		joinMessage = fmt.Sprintf("🟢 %s вернулся в чат", nickname)
	}

	s.broadcastMessage(joinMessage, client)
	fmt.Printf("✅ %s (%s) присоединился к чату\n", nickname, address)

	// Отправляем список пользователей новому клиенту
	s.sendUserList(client)

	// Обработка сообщений от клиента
	for s.running {
		message, err := reader.ReadString('\n')
		if err != nil {
			if s.running {
				fmt.Printf("❌ Ошибка чтения сообщения от %s: %v\n", nickname, err)
			}
			break
		}

		message = strings.TrimSpace(message)
		if message == "" {
			continue
		}

		// Обработка личных сообщений через @ник
		if strings.HasPrefix(message, "@") {
			parts := strings.SplitN(message, " ", 2)
			if len(parts) >= 2 {
				targetNick := strings.TrimPrefix(parts[0], "@")
				privateMsg := parts[1]

				if targetNick != "" && privateMsg != "" {
					// Проверяем, онлайн ли целевой пользователь
					targetClient := s.findClientByNickname(targetNick)

					if targetClient != nil && targetClient != client {
						// Пользователь онлайн - отправляем сразу
						// Проверяем блокировку
						if targetClient.blocked[client.nickname] {
							s.sendToClient(client, fmt.Sprintf("❌ Пользователь %s заблокировал вас", targetNick))
							continue
						}

						timestamp := time.Now().Format("15:04:05")
						privateMessage := fmt.Sprintf("[ЛС][%s] %s: %s", timestamp, client.nickname, privateMsg)
						confirmation := fmt.Sprintf("[ЛС][%s] Вы → %s: %s", timestamp, targetNick, privateMsg)

						s.sendToClient(targetClient, privateMessage)
						s.sendToClient(client, confirmation)
						fmt.Printf("💌 ЛС от %s к %s: %s\n", client.nickname, targetNick, privateMsg)
					} else {
						// Пользователь оффлайн - сохраняем как отложенное сообщение
						if targetNick == client.nickname {
							s.sendToClient(client, "❌ Нельзя отправить сообщение самому себе")
							continue
						}

						// Добавляем отложенное сообщение
						success := s.addOfflineMessage(targetNick, client.nickname, privateMsg)

						if success {
							timestamp := time.Now().Format("15:04:05")
							s.sendToClient(client, fmt.Sprintf("📮 [%s] Сообщение для %s сохранено (пользователь оффлайн)", timestamp, targetNick))
							fmt.Printf("📮 %s оставил сообщение для %s (оффлайн): %s\n", client.nickname, targetNick, privateMsg)
						} else {
							s.sendToClient(client, fmt.Sprintf("❌ Почтовый ящик %s переполнен (максимум 10 сообщений)", targetNick))
						}
					}
					continue
				}
			} else {
				s.sendToClient(client, "❌ Использование: @никнейм сообщение")
				continue
			}
		}

		// Обработка команды #mailbox
		if message == "#mailbox" {
			s.getMailboxStatus(client)
			continue
		}

		if strings.HasPrefix(message, "#") {
			parts := strings.SplitN(message, " ", 2)
			cmd := strings.ToLower(strings.TrimPrefix(parts[0], "#"))

			switch cmd {
			case "help":
				s.sendHelp(client)
				continue
			case "users":
				s.sendUserList(client)
				continue
			case "all":
				if len(parts) < 2 {
					s.sendToClient(client, "❌ Использование: #all сообщение")
					continue
				}
				msg := parts[1]
				timestamp := time.Now().Format("15:04:05")
				privateMessage := fmt.Sprintf("[МЛС][%s] %s: %s", timestamp, nickname, msg)
				s.broadcastPrivateMessage(privateMessage, client)
				s.sendToClient(client, fmt.Sprintf("[МЛС][%s] Вы: %s", timestamp, msg))
				continue
			case "block":
				if len(parts) < 2 {
					s.sendToClient(client, "❌ Использование: #block ник")
					continue
				}
				target := parts[1]
				client.blocked[target] = true
				s.sendToClient(client, fmt.Sprintf("🚫 %s добавлен в чёрный список", target))
				continue
			case "unblock":
				if len(parts) < 2 {
					s.sendToClient(client, "❌ Использование: #unblock ник")
					continue
				}
				target := parts[1]
				delete(client.blocked, target)
				s.sendToClient(client, fmt.Sprintf("✅ %s убран из чёрного списка", target))
				continue
			case "fav":
				if len(parts) < 2 {
					// Отправляем список в сыром виде
					if len(client.favoriteUsers) == 0 {
						s.sendToClient(client, "FAV_LIST:")
					} else {
						var favList []string
						for user := range client.favoriteUsers {
							favList = append(favList, user)
						}
						s.sendToClient(client, "FAV_LIST:"+strings.Join(favList, ","))
					}
					continue
				}

				target := parts[1]

				if strings.ToLower(target) == "clear" || target == "" {
					client.favoriteUsers = make(map[string]bool)
					s.sendToClient(client, "FAV_CLEARED")
				} else if strings.ToLower(target) == "list" {
					if len(client.favoriteUsers) == 0 {
						s.sendToClient(client, "FAV_LIST:")
					} else {
						var favList []string
						for user := range client.favoriteUsers {
							favList = append(favList, user)
						}
						s.sendToClient(client, "FAV_LIST:"+strings.Join(favList, ","))
					}
				} else if target == client.nickname {
					s.sendToClient(client, "FAV_ERROR:Нельзя добавить себя в любимые писатели")
				} else if s.isNicknameTaken(target) {
					if client.favoriteUsers[target] {
						delete(client.favoriteUsers, target)
						s.sendToClient(client, "FAV_REMOVED:"+target)
					} else {
						client.favoriteUsers[target] = true
						s.sendToClient(client, "FAV_ADDED:"+target)
					}
				} else {
					s.sendToClient(client, "FAV_ERROR:Пользователь "+target+" не найден")
				}
				continue

			}
		}

	}
}

func (s *ChatServer) broadcastPrivateMessage(message string, sender *Client) {
	s.mutex.Lock()
	clients := make([]*Client, len(s.clients))
	copy(clients, s.clients)
	s.mutex.Unlock()

	for _, client := range clients {
		if client == sender {
			continue
		}
		// Проверяем, не заблокирован ли отправитель получателем
		if client.blocked[sender.nickname] {
			continue // Пропускаем заблокированных
		}
		s.sendToClient(client, message)
	}
}

func (s *ChatServer) sendPrivateMessage(targetNickname, message string, sender *Client) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients {
		if client.nickname == targetNickname && client != sender {
			// Проверяем, не заблокирован ли отправитель получателем
			if client.blocked[sender.nickname] {
				return false // Получатель заблокировал отправителя
			}
			s.sendToClient(client, message)
			return true
		}
	}
	return false
}

func (s *ChatServer) sendToClient(client *Client, message string) {
	client.writer.WriteString(message + "\n")
	client.writer.Flush()
}

func (s *ChatServer) sendHelp(client *Client) {
	helpMessage := "HELP:" +
		"@ник сообщение - личное сообщение | " +
		"#all сообщение - массовое личное сообщение | " +
		"#users - список пользователей | " +
		"#help - эта справка | " +
		"#mailbox - проверить почтовый ящик | " +
		"#fav [ник] - добавить/удалить любимого писателя | " +
		"#fav list - показать список | " +
		"#fav clear - очистить список | " +
		"#block ник - добавить в чёрный список | " +
		"#unblock ник - убрать из чёрного списка | " +
		"/quit - выход из чата"
	s.sendToClient(client, helpMessage)
}
func (s *ChatServer) isNicknameTaken(nickname string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, client := range s.clients {
		if client.nickname == nickname {
			return true
		}
	}
	return false
}

func (s *ChatServer) addClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.clients = append(s.clients, client)
}

func (s *ChatServer) removeClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for i, c := range s.clients {
		if c == client {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			break
		}
	}
}

func (s *ChatServer) broadcastMessage(message string, exclude *Client) {
	s.mutex.Lock()
	clients := make([]*Client, len(s.clients))
	copy(clients, s.clients)
	s.mutex.Unlock()

	var disconnected []*Client

	for _, client := range clients {
		if exclude != nil && client == exclude {
			continue
		}

		_, err := client.writer.WriteString(message + "\n")
		if err != nil {
			fmt.Printf("❌ Ошибка отправки сообщения %s: %v\n", client.nickname, err)
			disconnected = append(disconnected, client)
		} else {
			client.writer.Flush()
		}
	}

	// Удаляем отключившихся клиентов
	for _, client := range disconnected {
		s.removeClient(client)
		fmt.Printf("🔴 %s отключился (потеряна связь)\n", client.nickname)
		client.conn.Close()
	}
}

func (s *ChatServer) sendUserList(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var userList strings.Builder
	userList.WriteString("USERS:")
	for i, c := range s.clients {
		if i > 0 {
			userList.WriteString(",")
		}
		userList.WriteString(c.nickname)
	}

	s.sendToClient(client, userList.String())
}

func (s *ChatServer) disconnectClient(client *Client) {
	s.removeClient(client)
	client.conn.Close()

	if client.nickname != "" {
		leaveMessage := fmt.Sprintf("🔴 %s покинул чат", client.nickname)
		s.broadcastMessage(leaveMessage, nil)
		fmt.Printf("👋 %s отключился\n", client.nickname)
	}
}

func (s *ChatServer) userExistsInHistory(nickname string) bool {
	s.historyMutex.RLock()
	defer s.historyMutex.RUnlock()

	for _, storedNickname := range s.userHistory {
		if storedNickname == nickname {
			return true
		}
	}
	return false
}

func (s *ChatServer) Shutdown() {
	if !s.running {
		return
	}

	s.running = false
	fmt.Println("\n🛑 Остановка сервера...")

	// Закрываем все клиентские соединения
	s.mutex.Lock()
	for _, client := range s.clients {
		client.conn.Close()
	}
	s.clients = nil
	s.mutex.Unlock()

	// Закрываем listener
	if s.listener != nil {
		s.listener.Close()
	}

	fmt.Println("✅ Сервер остановлен")
}

func (s *ChatServer) handleSignals() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n🛑 Получен сигнал остановки...")
	s.Shutdown()
	os.Exit(0)
}

func main() {
	fmt.Println("=== 💬 Многопользовательский чат-сервер (Go) ===")
	fmt.Println("Для остановки нажмите Ctrl+C")

	server := NewChatServer("0.0.0.0", 12345)

	err := server.Start()
	if err != nil {
		fmt.Printf("❌ Ошибка: %v\n", err)
		os.Exit(1)
	}
}
