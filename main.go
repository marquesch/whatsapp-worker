package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"net/url"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"github.com/streadway/amqp"
)

const (
	receiveMessageQueueName = "async_tasks"
	sendMessageQueueName    = "whatsapp_message"
)

var (
	amqpHost     = os.Getenv("RABBITMQ_HOST")
	amqpPort     = os.Getenv("RABBITMQ_PORT")
	amqpUser     = os.Getenv("RABBITMQ_USER")
	amqpPassword = url.PathEscape(os.Getenv("RABBITMQ_PASSWORD"))

	rabbitMQURL     = "amqp://" + amqpUser + ":" + amqpPassword + "@" + amqpHost + ":" + amqpPort + "/"
	rabbitMQChannel *amqp.Channel
)

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

type ProcessMessagePayload struct {
	Task    string                `json:"task"`
	Payload ReceiveMessagePayload `json:"kwargs"`
}

type SendMessagePayload struct {
	PhoneNumber string `json:"phone_number"`
	MessageBody string `json:"message_body"`
}

type ReceiveMessagePayload struct {
	MessageId   string `json:"message_id"`
	MessageBody string `json:"message_body"`
	PhoneNumber string `json:"phone_number"`
	Timestamp   string `json:"timestamp"`
}

func NewProcessMessagePayload() *ProcessMessagePayload {
	return &ProcessMessagePayload{Task: "ProcessIncomingMessage"}
}

func NewSendMessagePayload() *SendMessagePayload {
	return &SendMessagePayload{}
}

func setUpRabbitMQConsumer(channel *amqp.Channel) <-chan amqp.Delivery {
	log.Printf("Declaring queue: %s", sendMessageQueueName)
	_, err := channel.QueueDeclare(
		sendMessageQueueName,
		true,
		false,
		false,
		false,
		nil,
	)

	failOnError(err, "Error declaring queue")
	log.Printf("Successfully declared queue: %s", sendMessageQueueName)

	log.Printf("Starting consumer on queue: %s", sendMessageQueueName)
	messages, err := channel.Consume(
		sendMessageQueueName,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	failOnError(err, "Error starting consumer")
	log.Printf("Successfully started consumer on queue: %s", sendMessageQueueName)

	return messages
}

func handleSendWhatsappMessage(client *whatsmeow.Client, body []byte) {
	payload := NewSendMessagePayload()
	err := json.Unmarshal(body, &payload)
	if err != nil {
		log.Printf("ERROR: Failed to unmarshal payload: %v", err)
		return
	}

	log.Printf("Got a request to send message. Payload: %+v", payload)

	recipientJID := types.NewJID(payload.PhoneNumber, types.DefaultUserServer)

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(payload.MessageBody),
		},
	}

	resp, err := client.SendMessage(context.Background(), recipientJID, msg)
	if err != nil {
		log.Printf("ERROR: Failed to send message: %v", err)
		return
	}

	log.Printf("Successfully sent message. Response: %+v", resp)
}

func initRabbitMQ() *amqp.Channel {
	log.Printf("Connecting to RabbitMQ at: %s", rabbitMQURL)
	connection, err := amqp.Dial(rabbitMQURL)
	failOnError(err, "Could not connect to RabbitMQ host")
	log.Println("Successfully connected to RabbitMQ")

	channel, err := connection.Channel()
	failOnError(err, "Could not create RabbitMQ channel")
	log.Println("Successfully created RabbitMQ channel")

	return channel
}

func messageReceiveHandler(evt interface{}) {
	switch evt := evt.(type) {
	case *events.Message:
		var phoneNumber string
		if strings.Contains(evt.Info.Sender.Server, "lid") {
			phoneNumber = evt.Info.SenderAlt.User
		} else {
			phoneNumber = evt.Info.Sender.User
		}

		log.Printf("Received WhatsApp message event from: %s, IsFromMe: %v, IsGroup: %v, Type: %s",
			phoneNumber, evt.Info.IsFromMe, evt.Info.IsGroup, evt.Info.Type)

		if evt.Info.IsFromMe || evt.Info.IsGroup || evt.Info.Type != "text" {
			log.Printf("Skipping message (IsFromMe: %v, IsGroup: %v, Type: %s)",
				evt.Info.IsFromMe, evt.Info.IsGroup, evt.Info.Type)
			return
		}

		payload := NewProcessMessagePayload()
		payload.Payload.PhoneNumber = phoneNumber
		payload.Payload.MessageId = evt.Info.ID

		if evt.Message.Conversation != nil {
			payload.Payload.MessageBody = *evt.Message.Conversation
		} else if evt.Message.ExtendedTextMessage != nil {
			payload.Payload.MessageBody = evt.Message.ExtendedTextMessage.GetText()
		}

		payload.Payload.Timestamp = evt.Info.Timestamp.Format(time.RFC3339)

		msgData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("ERROR: Failed to marshal payload: %v", err)
			return
		}

		log.Printf("Publishing message to queue '%s': %s", receiveMessageQueueName, string(msgData))
		err = rabbitMQChannel.Publish(
			"",
			receiveMessageQueueName,
			false,
			false,
			amqp.Publishing{
				ContentType: "application/json",
				Body:        msgData,
			},
		)
		if err != nil {
			log.Printf("ERROR: Failed to publish message: %v", err)
			return
		}
		log.Printf("Successfully published message to queue '%s'", receiveMessageQueueName)
	}
}

func main() {
	log.Println("Starting WhatsApp Worker...")

	// Validate environment variables
	if amqpHost == "" || amqpPort == "" || amqpUser == "" || amqpPassword == "" {
		log.Fatal("Missing required environment variables: RABBITMQ_HOST, RABBITMQ_PORT, RABBITMQ_USER, RABBITMQ_PASSWORD")
	}

	log.Printf("RabbitMQ Config - Host: %s, Port: %s, User: %s", amqpHost, amqpPort, amqpUser)

	dbLog := waLog.Stdout("Database", "DEBUG", true)

	log.Println("Initializing SQLite database...")
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:storage/wadb.db?_foreign_keys=on", dbLog)
	failOnError(err, "Error creating db container")

	deviceStore, err := container.GetFirstDevice(context.Background())
	failOnError(err, "Error getting device")

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.AutomaticMessageRerequestFromPhone = true
	client.AddEventHandler(messageReceiveHandler)

	log.Println("Initializing RabbitMQ...")
	rabbitMQChannel = initRabbitMQ()

	if client.Store.ID == nil {
		log.Println("No stored credentials found, requesting QR code...")
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		failOnError(err, "Error connecting to client")

		for evt := range qrChan {
			if evt.Event == "code" {
				log.Println("QR Code generated, please scan:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		log.Printf("Using stored credentials for device: %s", client.Store.ID)
		err = client.Connect()
		failOnError(err, "Error connecting to client")
	}

	log.Println("Setting up RabbitMQ consumer...")
	messages := setUpRabbitMQConsumer(rabbitMQChannel)

	log.Println("Starting message processing goroutine...")
	go func() {
		log.Println("Message processing goroutine started, waiting for messages...")
		messageCount := 0
		for message := range messages {
			messageCount++
			log.Printf("Processing message #%d from queue", messageCount)
			handleSendWhatsappMessage(client, message.Body)
			err := message.Ack(false)
			if err != nil {
				log.Printf("ERROR: Failed to acknowledge message: %v", err)
			} else {
				log.Printf("Successfully acknowledged message #%d", messageCount)
			}
		}
		log.Println("WARNING: Message channel closed!")
	}()

	log.Println("WhatsApp Worker is ready and listening for messages")
	log.Println("Press Ctrl+C to shutdown gracefully")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Received shutdown signal, cleaning up...")
	client.Disconnect()
	log.Println("WhatsApp Worker stopped")
}
