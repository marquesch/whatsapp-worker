package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"github.com/streadway/amqp"
)

const (
	receiveMessageQueueName = "async_tasks"
	sendMessageQueueName    = "whatsapp_message"
)

var (
	amqpHost     = os.Getenv("AMQP_HOST")
	amqpPort     = os.Getenv("AMQP_PORT")
	amqpUser     = os.Getenv("AMQP_USER")
	amqpPassword = os.Getenv("AMQP_PASSWORD")

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
	_, err := channel.QueueDeclare(
		sendMessageQueueName,
		true,
		false,
		false,
		false,
		nil,
	)

	failOnError(err, "Error declaring queue")

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

	return messages
}

func handleSendWhatsappMessage(client *whatsmeow.Client, body []byte) {
	payload := NewSendMessagePayload()
	err := json.Unmarshal(body, &payload)
	failOnError(err, "Error unmarshalling payload")

	recipientJID := types.NewJID(payload.PhoneNumber, types.DefaultUserServer)

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(payload.MessageBody),
		},
	}

	_, err = client.SendMessage(context.Background(), recipientJID, msg)

	failOnError(err, "Error sending message")
}

func initRabbitMQ() *amqp.Channel {
	connection, err := amqp.Dial(rabbitMQURL)
	failOnError(err, "Could not connect to RabbitMQ host")

	channel, err := connection.Channel()
	failOnError(err, "Could not create RabbitMQ channel")

	return channel
}

func messageReceiveHandler(evt interface{}) {
	switch evt := evt.(type) {
	case *events.Message:
		if evt.Info.IsFromMe || evt.Info.IsGroup || evt.Info.Type != "text" {
			return
		}

		payload := NewProcessMessagePayload()
		payload.Payload.PhoneNumber = evt.Info.SenderAlt.User
		payload.Payload.MessageId = evt.Info.ID

		if evt.Message.Conversation != nil {
			payload.Payload.MessageBody = *evt.Message.Conversation
		} else if evt.Message.ExtendedTextMessage != nil {
			payload.Payload.MessageBody = evt.Message.ExtendedTextMessage.GetText()
		}

		payload.Payload.Timestamp = evt.Info.Timestamp.Format(time.RFC3339)

		msgData, err := json.Marshal(payload)
		failOnError(err, "Error marshalling payload")

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
		failOnError(err, "Error publishing message")
	}
}

func main() {
	dbLog := waLog.Stdout("Database", "DEBUG", true)

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:storage/wadb.db?_foreign_keys=on", dbLog)
	failOnError(err, "Error creating db container")

	deviceStore, err := container.GetFirstDevice(context.Background())
	failOnError(err, "Error getting device")

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.AutomaticMessageRerequestFromPhone = true
	client.AddEventHandler(messageReceiveHandler)

	rabbitMQChannel = initRabbitMQ()

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		failOnError(err, "Error connecting to client")

		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		err = client.Connect()
		failOnError(err, "Error connecting to client")
	}

	messages := setUpRabbitMQConsumer(rabbitMQChannel)

	go func() {
		for message := range messages {
			handleSendWhatsappMessage(client, message.Body)
			message.Ack(false)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}
