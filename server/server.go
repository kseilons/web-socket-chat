package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
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
	conn          *websocket.Conn
	nickname      string
	address       string
	send          chan Message
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
	clients      map[*Client]bool
	mutex        sync.Mutex
	running      bool
	userHistory  map[string]string
	historyMutex sync.RWMutex
	mailboxes    map[string]*Mailbox // никнейм -> почтовый ящик
	mailboxMutex sync.RWMutex
	upgrader     websocket.Upgrader
}

func NewChatServer(host string, port int) *ChatServer {
	return &ChatServer{
		host:        host,
		port:        port,
		clients:     make(map[*Client]bool),
		running:     false,
		userHistory: make(map[string]string),
		mailboxes:   make(map[string]*Mailbox),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Разрешаем подключения с любых источников
			},
		},
	}
}

// Функции для работы с WebSocket сообщениями
func (s *ChatServer) sendJSONMessage(client *Client, msg Message) error {
	select {
	case client.send <- msg:
		return nil
	default:
		close(client.send)
		return fmt.Errorf("канал отправки заблокирован")
	}
}

func (s *ChatServer) readJSONMessage(conn *websocket.Conn) (*Message, error) {
	_, message, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	var msg Message
	err = json.Unmarshal(message, &msg)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &msg, nil
}

func (s *ChatServer) Start() error {
	address := fmt.Sprintf("%s:%d", s.host, s.port)
	s.running = true

	// Настраиваем HTTP маршруты
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/", s.handleHome)

	fmt.Printf("🚀 WebSocket чат-сервер запущен на %s\n", address)
	fmt.Println("WebSocket endpoint: ws://" + address + "/ws")
	fmt.Println("Ожидание подключений...")

	// Обработка сигналов для graceful shutdown
	go s.handleSignals()

	// Запускаем HTTP сервер
	return http.ListenAndServe(address, nil)
}

func (s *ChatServer) handleHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <title>WebSocket Chat Server</title>
</head>
<body>
    <h1>WebSocket Chat Server</h1>
    <p>Сервер запущен и готов к подключениям</p>
    <p>WebSocket endpoint: <code>ws://%s:%d/ws</code></p>
</body>
</html>
`, s.host, s.port)
}

func (s *ChatServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ Ошибка обновления до WebSocket: %v", err)
		return
	}

	clientAddr := r.RemoteAddr
	fmt.Printf("📱 Новое WebSocket подключение: %s\n", clientAddr)

	// Создаем клиента
	client := &Client{
		conn:          conn,
		address:       clientAddr,
		send:          make(chan Message, 256),
		blocked:       make(map[string]bool),
		favoriteUsers: make(map[string]bool),
	}

	// Добавляем клиента в список
	s.addClient(client)

	// Запускаем горутины для чтения и записи
	go s.writePump(client)
	go s.readPump(client)
}

// formatFullIP приводит IP к полному виду.
// Для IPv4 возвращает обычную запись, для IPv6 расширяет сокращения (например, ::1 -> 0000:0000:0000:0000:0000:0000:0000:0001)
func formatFullIP(host string) string {
	// Убираем zone-id, если присутствует (например, fe80::1%lo0)
	if idx := strings.Index(host, "%"); idx >= 0 {
		host = host[:idx]
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return host
	}
	hextets := make([]string, 8)
	for i := 0; i < 8; i++ {
		hextets[i] = fmt.Sprintf("%04x", uint16(ip16[i*2])<<8|uint16(ip16[i*2+1]))
	}
	return strings.Join(hextets, ":")
}

// writePump отправляет сообщения клиенту
func (s *ChatServer) writePump(client *Client) {
	defer client.conn.Close()

	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}

			jsonData, err := json.Marshal(message)
			if err != nil {
				log.Printf("❌ Ошибка сериализации JSON: %v", err)
				return
			}

			w.Write(jsonData)

			// Закрываем writer
			if err := w.Close(); err != nil {
				return
			}
		}
	}
}

// readPump читает сообщения от клиента
func (s *ChatServer) readPump(client *Client) {
	defer func() {
		s.disconnectClient(client)
		client.conn.Close()
	}()

	// Сначала обрабатываем аутентификацию
	if !s.handleAuthentication(client) {
		return
	}

	// Основной цикл чтения сообщений
	for {
		msg, err := s.readJSONMessage(client.conn)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("❌ WebSocket ошибка: %v", err)
			}
			break
		}

		s.handleClientMessage(client, msg)
	}
}

func (s *ChatServer) handleAuthentication(client *Client) bool {
	// Извлекаем IP из адреса (корректно обрабатываем IPv6 в формате [::1]:port)
	host, _, err := net.SplitHostPort(client.address)
	if err != nil {
		host = client.address
	}
	ip := host

	// Проверяем историю для этого IP
	previousNickname := s.getPreviousNickname(ip)

	// Если есть предыдущий никнейм, предлагаем его
	if previousNickname != "" {
		msg := Message{
			Type:    "nick_prompt",
			Content: previousNickname,
		}
		s.sendJSONMessage(client, msg)
		fmt.Printf("📝 Предлагаем никнейм '%s' для IP %s\n", previousNickname, ip)
	} else {
		msg := Message{Type: "nick_request"}
		s.sendJSONMessage(client, msg)
	}

	// Читаем JSON сообщение с никнеймом
	nickMsg, err := s.readJSONMessage(client.conn)
	if err != nil {
		fmt.Printf("❌ Ошибка чтения никнейма от %s: %v\n", client.address, err)
		return false
	}

	if nickMsg.Type != "nick" {
		errorMsg := Message{
			Type:  "error",
			Error: "Ожидается сообщение с никнеймом",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	nickname := strings.TrimSpace(nickMsg.Content)
	if nickname == "" {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм не может быть пустым",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	// Проверяем, не занят ли никнейм
	if s.isNicknameTaken(nickname) {
		errorMsg := Message{
			Type:  "error",
			Error: "Никнейм уже занят",
		}
		s.sendJSONMessage(client, errorMsg)
		return false
	}

	// Сохраняем в историю
	s.saveNicknameHistory(ip, nickname)

	// Устанавливаем никнейм клиента
	client.nickname = nickname

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
	fmt.Printf("✅ %s (%s) присоединился к чату\n", nickname, client.address)

	// Отправляем список пользователей новому клиенту
	s.sendUserListJSON(client)

	return true
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

	for client := range s.clients {
		if client.nickname == nickname {
			return client
		}
	}
	return nil
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
			privateMsg := Message{
				Type:      "private",
				Content:   msg.Content,
				From:      client.nickname,
				To:        msg.To,
				Timestamp: timestamp,
				Flags:     map[string]bool{"private": true},
			}

			// Добавляем флаг "favorite" если отправитель в списке любимых получателя
			if targetClient.favoriteUsers[client.nickname] {
				privateMsg.Flags["favorite"] = true
			}

			s.sendJSONMessage(targetClient, privateMsg)
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
	case "ip":
		target := msg.Data["target"]
		if target == "" {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Использование: #ip ник",
			})
			return
		}
		targetClient := s.findClientByNickname(target)
		if targetClient == nil {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: fmt.Sprintf("Пользователь %s не найден или оффлайн", target),
			})
			return
		}
		host, _, err := net.SplitHostPort(targetClient.address)
		if err != nil {
			host = targetClient.address
		}
		parsed := net.ParseIP(host)
		if parsed == nil {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: "Не удалось определить IP",
			})
			return
		}
		var ipOut string
		if v4 := parsed.To4(); v4 != nil {
			ipOut = v4.String()
		} else if parsed.IsLoopback() {
			// Принудительно возвращаем IPv4 для loopback
			ipOut = "127.0.0.1"
		} else {
			s.sendJSONMessage(client, Message{
				Type:  "error",
				Error: fmt.Sprintf("Для пользователя %s доступен только IPv6, IPv4 отсутствует", target),
			})
			return
		}
		s.sendJSONMessage(client, Message{
			Type:    "ip",
			Content: fmt.Sprintf("IPv4: %s", ipOut),
			Data:    map[string]string{"user": target, "ip": ipOut, "family": "IPv4"},
		})
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

func (s *ChatServer) broadcastJSONMessage(msg Message, exclude *Client) {
	s.mutex.Lock()
	clients := make(map[*Client]bool)
	for client := range s.clients {
		clients[client] = true
	}
	s.mutex.Unlock()

	var disconnected []*Client

	for client := range clients {
		if exclude != nil && client == exclude {
			continue
		}

		// Проверяем блокировку для личных и массовых сообщений
		if (msg.Type == "private" || msg.Type == "mass_private") && client.blocked[msg.From] {
			continue
		}

		// Создаем копию сообщения для каждого клиента
		clientMsg := msg

		// Добавляем флаг "favorite" если отправитель в списке любимых получателя
		if (msg.Type == "chat" || msg.Type == "mass_private") && client.favoriteUsers[msg.From] {
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
		"#ip ник":        "узнать IP пользователя",
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

	for client := range s.clients {
		if client.nickname == nickname {
			return true
		}
	}
	return false
}

func (s *ChatServer) addClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.clients[client] = true
}

func (s *ChatServer) removeClient(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.clients, client)
}

func (s *ChatServer) sendUserListJSON(client *Client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var users []string
	for c := range s.clients {
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
	for client := range s.clients {
		close(client.send)
		client.conn.Close()
	}
	s.clients = make(map[*Client]bool)
	s.mutex.Unlock()

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

func getServerConfig() (string, int) {
	// Проверяем переменные окружения
	host := os.Getenv("SERVER_HOST")
	portStr := os.Getenv("SERVER_PORT")

	// Если переменные окружения не заданы, используем интерактивный ввод
	if host == "" && portStr == "" {
		reader := bufio.NewReader(os.Stdin)

		fmt.Println("=== 💬 WebSocket чат-сервер (Go) ===")
		fmt.Println("Введите адрес сервера (по умолчанию: 0.0.0.0)")
		fmt.Print("Адрес сервера [0.0.0.0]: ")

		hostInput, _ := reader.ReadString('\n')
		hostInput = strings.TrimSpace(hostInput)

		if hostInput == "" {
			hostInput = "0.0.0.0"
		}

		fmt.Print("Введите порт сервера (по умолчанию: 12345): ")
		portInput, _ := reader.ReadString('\n')
		portInput = strings.TrimSpace(portInput)

		var port int
		if portInput == "" {
			port = 12345
		} else {
			if _, err := fmt.Sscanf(portInput, "%d", &port); err != nil || port <= 0 || port >= 65536 {
				fmt.Printf("❌ Неверный порт, используем порт по умолчанию: 12345\n")
				port = 12345
			}
		}

		return hostInput, port
	}

	// Используем переменные окружения
	if host == "" {
		host = "0.0.0.0"
	}

	var port int
	if portStr == "" {
		port = 12345
	} else {
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 || port >= 65536 {
			fmt.Printf("❌ Неверный порт в переменной окружения SERVER_PORT, используем порт по умолчанию: 12345\n")
			port = 12345
		}
	}

	fmt.Println("=== 💬 WebSocket чат-сервер (Go) ===")
	fmt.Printf("Используются настройки из переменных окружения:\n")
	fmt.Printf("Адрес сервера: %s\n", host)
	fmt.Printf("Порт сервера: %d\n", port)

	return host, port
}

func main() {
	host, port := getServerConfig()

	fmt.Printf("🚀 Запуск сервера на %s:%d\n", host, port)
	fmt.Println("Для остановки нажмите Ctrl+C")
	fmt.Println()

	server := NewChatServer(host, port)

	err := server.Start()
	if err != nil {
		fmt.Printf("❌ Ошибка: %v\n", err)
		os.Exit(1)
	}
}
