package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// JSON структуры для сообщений
type Message struct {
	Type      string            `json:"type"`
	Content   string            `json:"content,omitempty"`
	From      string            `json:"from,omitempty"`
	To        string            `json:"to,omitempty"`
	Timestamp string            `json:"timestamp,omitempty"`
	Users     []string          `json:"users,omitempty"`
	Flags     map[string]bool   `json:"flags,omitempty"`
	Error     string            `json:"error,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

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

// Функции для работы с JSON сообщениями
func (s *ChatServer) sendJSONMessage(client *Client, msg Message) error {
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ошибка сериализации JSON: %v", err)
	}

	_, err = client.writer.WriteString(string(jsonData) + "\n")
	if err != nil {
		return err
	}
	return client.writer.Flush()
}

func (s *ChatServer) readJSONMessage(reader *bufio.Reader) (*Message, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}

	line = strings.TrimSpace(line)
	var msg Message
	err = json.Unmarshal([]byte(line), &msg)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &msg, nil
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
		s.sendJSONMessage(client, Message{
			Type:      "offline_message",
			Content:   msg.Message,
			From:      msg.From,
			Timestamp: timestamp,
			Flags:     map[string]bool{"offline": true},
		})
	}

	// Уведомляем пользователя
	s.sendJSONMessage(client, Message{
		Type:    "offline_delivered",
		Content: fmt.Sprintf("Вам доставлено %d отложенных сообщений", len(mailbox.Messages)),
	})

	// Очищаем ящик после доставки
	mailbox.Messages = make([]MailboxMessage, 0)
}

func (s *ChatServer) getMailboxStatusJSON(client *Client) {
	mailbox := s.getOrCreateMailbox(client.nickname)
	mailbox.Mutex.RLock()
	defer mailbox.Mutex.RUnlock()

	count := len(mailbox.Messages)
	if count == 0 {
		s.sendJSONMessage(client, Message{
			Type:    "mailbox_status",
			Content: "Ваш почтовый ящик пуст",
		})
	} else {
		s.sendJSONMessage(client, Message{
			Type:    "mailbox_status",
			Content: fmt.Sprintf("У вас %d отложенных сообщений", count),
		})
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
		msg := Message{
			Type:    "nick_prompt",
			Content: previousNickname,
		}
		s.sendJSONMessage(&Client{writer: writer}, msg)
		fmt.Printf("📝 Предлагаем никнейм '%s' для IP %s\n", previousNickname, ip)
	} else {
		msg := Message{Type: "nick_request"}
		s.sendJSONMessage(&Client{writer: writer}, msg)
	}

	// Читаем JSON сообщение с никнеймом
	nickMsg, err := s.readJSONMessage(reader)
	if err != nil {
		fmt.Printf("❌ Ошибка чтения никнейма от %s: %v\n", address, err)
		return
	}

	if nickMsg.Type != "nick" {
		errorMsg := Message{
			Type:  "error",
			Error: "Ожидается сообщение с никнеймом",
		}
		s.sendJSONMessage(&Client{writer: writer}, errorMsg)
		return
	}

	nickname = strings.TrimSpace(nickMsg.Content)
	if nickname == "" {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм не может быть пустым",
		}
		s.sendJSONMessage(&Client{writer: writer}, errorMsg)
		return
	}

	// Проверяем, не занят ли никнейм
	if s.isNicknameTaken(nickname) {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм уже занят",
		}
		s.sendJSONMessage(&Client{writer: writer}, errorMsg)
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

	// Отправляем подтверждение
	successMsg := Message{Type: "nick_ok"}
	s.sendJSONMessage(client, successMsg)

	// ТОЛЬКО ПОСЛЕ успешной аутентификации доставляем отложенные сообщения
	s.deliverOfflineMessages(client)

	// Уведомляем всех о новом пользователе
	joinMessage := fmt.Sprintf("🟢 %s присоединился к чату", nickname)

	// Добавляем информацию о повторном входе, если применимо
	if previousNickname != "" && previousNickname == nickname {
		joinMessage = fmt.Sprintf("🟢 %s вернулся в чат", nickname)
	}

	s.broadcastJSONMessage(Message{
		Type:      "system",
		Content:   joinMessage,
		Timestamp: time.Now().Format("15:04:05"),
	}, client)
	fmt.Printf("✅ %s (%s) присоединился к чату\n", nickname, address)

	// Отправляем список пользователей новому клиенту
	s.sendUserListJSON(client)

	// Обработка сообщений от клиента
	for s.running {
		msg, err := s.readJSONMessage(reader)
		if err != nil {
			if s.running {
				fmt.Printf("❌ Ошибка чтения сообщения от %s: %v\n", nickname, err)
			}
			break
		}

		s.handleClientMessage(client, msg)
	}
}

func (s *ChatServer) handleClientMessage(client *Client, msg *Message) {
	switch msg.Type {
	case "message":
		// Обычное сообщение в чат
		s.broadcastJSONMessage(Message{
			Type:      "chat",
			Content:   msg.Content,
			From:      client.nickname,
			Timestamp: time.Now().Format("15:04:05"),
			Flags:     msg.Flags,
		}, client)

	case "private":
		// Личное сообщение
		targetClient := s.findClientByNickname(msg.To)
		if targetClient != nil && targetClient != client {
			// Проверяем блокировку
			if targetClient.blocked[client.nickname] {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: fmt.Sprintf("Пользователь %s заблокировал вас", msg.To),
				})
				return
			}

			timestamp := time.Now().Format("15:04:05")
			// Отправляем получателю
			s.sendJSONMessage(targetClient, Message{
				Type:      "private",
				Content:   msg.Content,
				From:      client.nickname,
				To:        msg.To,
				Timestamp: timestamp,
				Flags:     map[string]bool{"private": true},
			})
			// Отправляем подтверждение отправителю
			s.sendJSONMessage(client, Message{
				Type:      "private_sent",
				Content:   msg.Content,
				From:      client.nickname,
				To:        msg.To,
				Timestamp: timestamp,
				Flags:     map[string]bool{"private": true},
			})
			fmt.Printf("💌 ЛС от %s к %s: %s\n", client.nickname, msg.To, msg.Content)
		} else {
			// Пользователь оффлайн - сохраняем как отложенное сообщение
			if msg.To == client.nickname {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Нельзя отправить сообщение самому себе",
				})
				return
			}

			success := s.addOfflineMessage(msg.To, client.nickname, msg.Content)
			if success {
				timestamp := time.Now().Format("15:04:05")
				s.sendJSONMessage(client, Message{
					Type:      "offline_saved",
					Content:   fmt.Sprintf("Сообщение для %s сохранено (пользователь оффлайн)", msg.To),
					Timestamp: timestamp,
				})
				fmt.Printf("📮 %s оставил сообщение для %s (оффлайн): %s\n", client.nickname, msg.To, msg.Content)
			} else {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: fmt.Sprintf("Почтовый ящик %s переполнен (максимум 10 сообщений)", msg.To),
				})
			}
		}

	case "command":
		s.handleCommand(client, msg)

	default:
		s.sendJSONMessage(client, Message{
			Type:  "error",
			Error: "Неизвестный тип сообщения",
		})
	}
}

func (s *ChatServer) handleCommand(client *Client, msg *Message) {
	cmd := msg.Data["command"]

	switch cmd {
	case "help":
		s.sendHelpJSON(client)
	case "users":
		s.sendUserListJSON(client)
	case "mailbox":
		s.getMailboxStatusJSON(client)
	case "all":
		content := msg.Data["content"]
		if content == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #all сообщение",
			})
			return
		}
		timestamp := time.Now().Format("15:04:05")
		s.broadcastJSONMessage(Message{
			Type:      "mass_private",
			Content:   content,
			From:      client.nickname,
			Timestamp: timestamp,
			Flags:     map[string]bool{"mass_private": true},
		}, client)
		s.sendJSONMessage(client, Message{
			Type:      "mass_private_sent",
			Content:   content,
			From:      client.nickname,
			Timestamp: timestamp,
			Flags:     map[string]bool{"mass_private": true},
		})

	case "block":
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #block ник",
			})
			return
		}
		client.blocked[target] = true
		s.sendJSONMessage(client, Message{
			Type:    "blocked",
			Content: fmt.Sprintf("%s добавлен в чёрный список", target),
		})

	case "unblock":
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #unblock ник",
			})
			return
		}
		delete(client.blocked, target)
		s.sendJSONMessage(client, Message{
			Type:    "unblocked",
			Content: fmt.Sprintf("%s убран из чёрного списка", target),
		})

	case "fav":
		action := msg.Data["action"]
		target := msg.Data["target"]

		switch action {
		case "list":
			if len(client.favoriteUsers) == 0 {
				s.sendJSONMessage(client, Message{
					Type:    "fav_list",
					Users:   []string{},
					Content: "Ваш список любимых писателей пуст",
				})
			} else {
				var favList []string
				for user := range client.favoriteUsers {
					favList = append(favList, user)
				}
				s.sendJSONMessage(client, Message{
					Type:    "fav_list",
					Users:   favList,
					Content: fmt.Sprintf("Ваши любимые писатели (%d): %s", len(favList), strings.Join(favList, ", ")),
				})
			}
		case "clear":
			client.favoriteUsers = make(map[string]bool)
			s.sendJSONMessage(client, Message{
				Type:    "fav_cleared",
				Content: "Список любимых писателей очищен",
			})
		case "add", "remove":
			if target == "" {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Укажите никнейм пользователя",
				})
				return
			}

			if target == client.nickname {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: "Нельзя добавить себя в любимые писатели",
				})
				return
			}

			if !s.isNicknameTaken(target) {
				s.sendJSONMessage(client, Message{
					Type:  "error",
					Error: fmt.Sprintf("Пользователь %s не найден", target),
				})
				return
			}

			if action == "add" {
				if client.favoriteUsers[target] {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("%s уже в списке любимых писателей", target),
					})
				} else {
					client.favoriteUsers[target] = true
					s.sendJSONMessage(client, Message{
						Type:    "fav_added",
						Content: fmt.Sprintf("%s добавлен в список любимых писателей", target),
						Data:    map[string]string{"user": target},
					})
				}
			} else { // remove
				if !client.favoriteUsers[target] {
					s.sendJSONMessage(client, Message{
						Type:  "error",
						Error: fmt.Sprintf("%s не в списке любимых писателей", target),
					})
				} else {
					delete(client.favoriteUsers, target)
					s.sendJSONMessage(client, Message{
						Type:    "fav_removed",
						Content: fmt.Sprintf("%s удален из списка любимых писателей", target),
						Data:    map[string]string{"user": target},
					})
				}
			}
		default:
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Неизвестная команда fav",
			})
		}

	default:
		s.sendJSONMessage(client, Message{
			Type:  "error",
			Error: "Неизвестная команда",
		})
	}
}

func (s *ChatServer) sendToClient(client *Client, message string) {
	client.writer.WriteString(message + "\n")
	client.writer.Flush()
}

func (s *ChatServer) broadcastJSONMessage(msg Message, exclude *Client) {
	s.mutex.Lock()
	clients := make([]*Client, len(s.clients))
	copy(clients, s.clients)
	s.mutex.Unlock()

	var disconnected []*Client

	for _, client := range clients {
		if exclude != nil && client == exclude {
			continue
		}

		// Проверяем блокировку для личных сообщений
		if msg.Type == "private" && client.blocked[msg.From] {
			continue
		}

		// Создаем копию сообщения для каждого клиента
		clientMsg := msg

		// Добавляем флаг "favorite" если отправитель в списке любимых получателя
		if msg.Type == "chat" && client.favoriteUsers[msg.From] {
			if clientMsg.Flags == nil {
				clientMsg.Flags = make(map[string]bool)
			}
			clientMsg.Flags["favorite"] = true
		}

		err := s.sendJSONMessage(client, clientMsg)
		if err != nil {
			fmt.Printf("❌ Ошибка отправки сообщения %s: %v\n", client.nickname, err)
			disconnected = append(disconnected, client)
		}
	}

	// Удаляем отключившихся клиентов
	for _, client := range disconnected {
		s.removeClient(client)
		fmt.Printf("🔴 %s отключился (потеряна связь)\n", client.nickname)
		client.conn.Close()
	}
}

func (s *ChatServer) sendHelpJSON(client *Client) {
	helpData := map[string]string{
		"@ник сообщение": "личное сообщение",
		"#all сообщение": "массовое личное сообщение",
		"#users":         "список пользователей",
		"#help":          "эта справка",
		"#mailbox":       "проверить почтовый ящик",
		"#fav [ник]":     "добавить/удалить любимого писателя",
		"#fav list":      "показать список",
		"#fav clear":     "очистить список",
		"#block ник":     "добавить в чёрный список",
		"#unblock ник":   "убрать из чёрного списка",
		"/quit":          "выход из чата",
	}

	s.sendJSONMessage(client, Message{
		Type: "help",
		Data: helpData,
	})
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

func (s *ChatServer) sendUserListJSON(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var users []string
	for _, c := range s.clients {
		users = append(users, c.nickname)
	}

	s.sendJSONMessage(client, Message{
		Type:  "users",
		Users: users,
	})
}

func (s *ChatServer) disconnectClient(client *Client) {
	s.removeClient(client)
	client.conn.Close()

	if client.nickname != "" {
		leaveMessage := fmt.Sprintf("🔴 %s покинул чат", client.nickname)
		s.broadcastJSONMessage(Message{
			Type:      "system",
			Content:   leaveMessage,
			Timestamp: time.Now().Format("15:04:05"),
		}, nil)
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
