package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type ChatClient struct {
	conn          net.Conn
	nickname      string
	server        string
	port          int
	running       bool
	reader        *bufio.Reader
	writer        *bufio.Writer
	favoriteUser  string
	consoleReader *bufio.Reader
	blocked       map[string]bool // локальный чёрный список
	favoriteUsers map[string]bool
}

func NewChatClient(server string, port int) *ChatClient {
	return &ChatClient{
		server:        server,
		port:          port,
		running:       true,
		consoleReader: bufio.NewReader(os.Stdin),
		blocked:       make(map[string]bool),
		favoriteUsers: make(map[string]bool),
	}
}

func (c *ChatClient) Connect() error {
	address := fmt.Sprintf("%s:%d", c.server, c.port)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к серверу: %v", err)
	}

	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = bufio.NewWriter(conn)

	fmt.Printf("✅ Подключено к серверу %s\n", address)
	return nil
}

func (c *ChatClient) Login() error {
	// Читаем первоначальный ответ от сервера
	initialResponse, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("ошибка получения приветствия от сервера: %v", err)
	}

	initialResponse = strings.TrimSpace(initialResponse)

	var nickname string

	if strings.HasPrefix(initialResponse, "NICK_PROMPT:") {
		// Сервер предлагает предыдущий никнейм
		suggestedNick := strings.TrimPrefix(initialResponse, "NICK_PROMPT:")
		suggestedNick = strings.TrimSpace(suggestedNick)

		fmt.Printf("🕒 Найден ваш предыдущий никнейм: %s\n", suggestedNick)
		fmt.Print("Нажмите Enter чтобы использовать его, или введите новый никнейм: ")

		input, err := c.consoleReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("ошибка чтения ввода: %v", err)
		}

		input = strings.TrimSpace(input)
		if input == "" {
			nickname = suggestedNick
			fmt.Printf("✅ Используем предыдущий никнейм: %s\n", nickname)
		} else {
			nickname = input
		}
	} else if initialResponse == "NICK_REQUEST" {
		// Сервер запрашивает новый никнейм
		fmt.Print("Введите ваш никнейм: ")
		input, err := c.consoleReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("ошибка чтения никнейма: %v", err)
		}
		nickname = strings.TrimSpace(input)
	} else {
		return fmt.Errorf("неожиданный ответ от сервера: %s", initialResponse)
	}

	// Отправляем выбранный никнейм серверу
	c.nickname = nickname
	nickMsg := fmt.Sprintf("NICK:%s\n", nickname)
	_, err = c.writer.WriteString(nickMsg)
	if err != nil {
		return fmt.Errorf("ошибка отправки никнейма: %v", err)
	}
	c.writer.Flush()

	// Получаем подтверждение от сервера
	response, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("ошибка получения ответа от сервера: %v", err)
	}

	response = strings.TrimSpace(response)
	if response == "NICK_TAKEN" {
		return fmt.Errorf("никнейм '%s' уже занят", nickname)
	}

	if response != "NICK_OK" {
		return fmt.Errorf("неожиданный ответ от сервера: %s", response)
	}

	fmt.Println("✅ Никнейм принят сервером")
	return nil
}

func (c *ChatClient) Start() {
	go c.readMessages()

	fmt.Println("\n💬 Добро пожаловать в чат!")
	fmt.Println("Доступные команды:")
	fmt.Println("  #help - показать справку")
	fmt.Println("  #users - список пользователей")
	fmt.Println("  #fav [ник] - добавить/удалить любимого писателя")
	fmt.Println("  #fav list - показать список любимых")
	fmt.Println("  #fav clear - очистить список")
	fmt.Println("  #all сообщение - массовое личное сообщение")
	fmt.Println("  @ник сообщение - приватное сообщение")
	fmt.Println("  #mailbox - проверить почтовый ящик")
	fmt.Println("  #block ник - добавить в чёрный список")
	fmt.Println("  #unblock ник - убрать из чёрного списка")
	fmt.Println("  /quit - выход из чата")
	fmt.Println(strings.Repeat("=", 50))

	for c.running {
		fmt.Print("> ")
		message, err := c.consoleReader.ReadString('\n')
		if err != nil {
			fmt.Printf("❌ Ошибка чтения ввода: %v\n", err)
			continue
		}

		message = strings.TrimSpace(message)

		if message == "/quit" {
			fmt.Println("👋 Выход из чата...")
			c.running = false
			break
		}

		// Обработка команды #fav на клиенте
		if strings.HasPrefix(message, "#fav") {
			// Просто отправляем команду на сервер, ответ придет в readMessages
			_, err = c.writer.WriteString(message + "\n")
			if err != nil {
				fmt.Printf("❌ Ошибка отправки команды: %v\n", err)
			} else {
				c.writer.Flush()
			}
			continue
		}

		if message == "" {
			continue
		}

		_, err = c.writer.WriteString(message + "\n")
		if err != nil {
			fmt.Printf("❌ Ошибка отправки сообщения: %v\n", err)
			c.running = false
			break
		}
		c.writer.Flush()
	}

	c.cleanup()
}

func (c *ChatClient) readMessages() {
	for c.running {
		message, err := c.reader.ReadString('\n')
		if err != nil {
			if c.running {
				fmt.Printf("\n❌ Ошибка чтения сообщения: %v\n", err)
				fmt.Println("Сервер недоступен. Отключение...")
			}
			c.running = false
			break
		}

		message = strings.TrimSpace(message)

		// Обработка спец сообщений
		if strings.HasPrefix(message, "USERS:") {
			c.handleUserList(message)
			continue
		}

		if strings.HasPrefix(message, "[📮]") {
			c.printFormattedMessage(message)
			continue
		}

		if strings.HasPrefix(message, "HELP:") {
			c.handleHelp(message)
			continue
		}

		// Обработка FAV_ сообщений от сервера
		if strings.HasPrefix(message, "FAV_") {
			c.handleFavResponse(message)
			continue
		}

		// Проверка на блокировку
		if c.isMessageBlocked(message) {
			continue
		}

		// Подсветка сообщений
		c.printFormattedMessage(message)
	}
}

func (c *ChatClient) handleFavResponse(message string) {
	if strings.HasPrefix(message, "FAV_LIST:") {
		favListStr := strings.TrimPrefix(message, "FAV_LIST:")
		if favListStr == "" {
			fmt.Println("📝 Ваш список любимых писателей пуст")
		} else {
			favList := strings.Split(favListStr, ",")
			// Обновляем локальный список
			c.favoriteUsers = make(map[string]bool)
			for _, user := range favList {
				if user != "" {
					c.favoriteUsers[user] = true
				}
			}
			fmt.Printf("❤️ Ваши любимые писатели (%d): %s\n", len(favList), strings.Join(favList, ", "))
		}
	} else if message == "FAV_CLEARED" {
		c.favoriteUsers = make(map[string]bool)
		fmt.Println("✅ Список любимых писателей очищен")
	} else if strings.HasPrefix(message, "FAV_ADDED:") {
		target := strings.TrimPrefix(message, "FAV_ADDED:")
		c.favoriteUsers[target] = true
		fmt.Printf("❤️ %s добавлен в список любимых писателей\n", target)
	} else if strings.HasPrefix(message, "FAV_REMOVED:") {
		target := strings.TrimPrefix(message, "FAV_REMOVED:")
		delete(c.favoriteUsers, target)
		fmt.Printf("✅ %s удален из списка любимых писателей\n", target)
	} else if strings.HasPrefix(message, "FAV_ERROR:") {
		errorMsg := strings.TrimPrefix(message, "FAV_ERROR:")
		fmt.Printf("❌ %s\n", errorMsg)
	}
	fmt.Print("> ")
}

func (c *ChatClient) isMessageBlocked(message string) bool {
	for blockedUser := range c.blocked {
		if strings.Contains(message, blockedUser) {
			return true
		}
	}
	return false
}

func (c *ChatClient) printFormattedMessage(message string) {
	// Проверяем, является ли отправитель любимым писателем
	isFavorite := false
	var messageSender string

	// Пытаемся извлечь отправителя из сообщения (формат: "никнейм: сообщение")
	if strings.Contains(message, ":") {
		// Для обычных сообщений: "никнейм: сообщение"
		if !strings.HasPrefix(message, "[") {
			parts := strings.SplitN(message, ":", 2)
			if len(parts) >= 2 {
				messageSender = strings.TrimSpace(parts[0])
				isFavorite = c.favoriteUsers[messageSender]
			}
		} else {
			// Для сообщений с префиксом: "[МЛС][время] никнейм: сообщение" или "[ЛС][время] никнейм: сообщение"
			// Ищем часть после последнего "]" которая содержит никнейм
			lastBracket := strings.LastIndex(message, "]")
			if lastBracket != -1 && lastBracket+1 < len(message) {
				// Берем часть после последнего "]"
				textAfterBracket := message[lastBracket+1:]
				// Убираем пробелы в начале и ищем никнейм до двоеточия
				textAfterBracket = strings.TrimSpace(textAfterBracket)
				if strings.Contains(textAfterBracket, ":") {
					parts := strings.SplitN(textAfterBracket, ":", 2)
					if len(parts) >= 2 {
						messageSender = strings.TrimSpace(parts[0])
						isFavorite = c.favoriteUsers[messageSender]
					}
				}
			}
		}
	}

	// Определяем тип сообщения и цвет
	switch {
	case isFavorite:
		// Сообщение от любимого писателя - специальное оформление (ВЫСШИЙ ПРИОРИТЕТ)
		fmt.Printf("\n\033[1;33m✨ %s\033[0m\n> ", message) // золотой с эмодзи
	case strings.HasPrefix(message, "[ЛС]"):
		fmt.Printf("\n\033[36m%s\033[0m\n> ", message) // голубой
	case strings.HasPrefix(message, "[МЛС]"):
		fmt.Printf("\n\033[35m%s\033[0m\n> ", message) // фиолетовый
	default:
		// Обычное сообщение
		fmt.Printf("\n%s\n> ", message)
	}
}

func (c *ChatClient) handleUserList(message string) {
	users := strings.TrimPrefix(message, "USERS:")
	userList := strings.Split(users, ",")

	fmt.Printf("\n👥 Пользователи онлайн (%d):\n", len(userList))
	for _, user := range userList {
		if user != "" {
			status := "🟢"
			if user == c.nickname {
				status = "🟡 (вы)"
			} else if c.favoriteUsers[user] {
				status = "❤️ " // любимый писатель
			}
			fmt.Printf("%s %s\n", status, user)
		}
	}

	// Показываем статистику по любимым писателям
	favCount := len(c.favoriteUsers)
	if favCount > 0 {
		var favList []string
		for user := range c.favoriteUsers {
			favList = append(favList, user)
		}
		fmt.Printf("❤️  Ваши любимые писатели (%d): %s\n", favCount, strings.Join(favList, ", "))
	}

	fmt.Print("> ")
}

func (c *ChatClient) handleHelp(message string) {
	helpText := strings.TrimPrefix(message, "HELP:")

	// Разбиваем текст по разделителю " | " для красивого отображения
	commands := strings.Split(helpText, " | ")

	fmt.Printf("\n\033[1;34m%s\033[0m\n", "📖 Справка по командам чата:")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println("📖 Справка по командам:")

	for _, cmd := range commands {

		// Разделяем команду и описание
		if strings.Contains(cmd, " - ") {
			parts := strings.SplitN(cmd, " - ", 2)
			fmt.Printf("\033[1;32m%-25s\033[0m %s\n", parts[0], parts[1])
		} else {
			fmt.Printf("  %s\n", cmd)
		}
	}

	fmt.Println(strings.Repeat("─", 60))
	fmt.Print("> ")
}

func (c *ChatClient) cleanup() {
	if c.conn != nil {
		c.conn.Close()
	}
	fmt.Println("✅ Соединение закрыто")
}

func (c *ChatClient) WaitForInterrupt() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n🛑 Получен сигнал прерывания...")
	c.running = false
	c.cleanup()
	os.Exit(0)
}

func getServerAddress() (string, int) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("=== 💬 Go клиент для чат-сервера ===")
	fmt.Println("Введите адрес сервера (по умолчанию: localhost:12345)")
	fmt.Print("Адрес сервера [localhost]: ")

	serverInput, _ := reader.ReadString('\n')
	serverInput = strings.TrimSpace(serverInput)

	if serverInput == "" {
		return "localhost", 12345
	}

	if strings.Contains(serverInput, ":") {
		parts := strings.Split(serverInput, ":")
		if len(parts) == 2 {
			server := parts[0]
			var port int
			if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil && port > 0 && port < 65536 {
				return server, port
			}
		}
	}

	return serverInput, 12345
}

func main() {
	server, port := getServerAddress()

	fmt.Printf("Подключение к %s:%d...\n", server, port)

	client := NewChatClient(server, port)
	go client.WaitForInterrupt()

	err := client.Connect()
	if err != nil {
		fmt.Printf("❌ Ошибка подключения: %v\n", err)
		os.Exit(1)
	}
	defer client.cleanup()

	err = client.Login()
	if err != nil {
		fmt.Printf("❌ Ошибка входа: %v\n", err)
		os.Exit(1)
	}

	client.Start()
}
