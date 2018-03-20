package models

import (
	"crypto/tls"
	"net"
	"context"
	"fmt"

	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/ApplicationInsights-Go/appinsights"
    "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	amqp10 "pack.ag/amqp"
	amqp091 "github.com/streadway/amqp"
)

// Order represents the order json
type Order struct {
	ID                string  `required:"false" description:"CosmoDB ID - will be autogenerated"`
	EmailAddress      string  `required:"true" description:"Email address of the customer"`
	PreferredLanguage string  `required:"false" description:"Preferred Language of the customer"`
	Product           string  `required:"false" description:"Product ordered by the customer"`
	Total             float64 `required:"false" description:"Order total"`
	Source            string  `required:"false" description:"Source backend e.g. App Service, Container instance, K8 cluster etc"`
	Status            string  `required:"true" description:"Order Status"`
}

// Environment variables
var customInsightsKey = os.Getenv("APPINSIGHTS_KEY")
var challengeInsightsKey = os.Getenv("CHALLENGEAPPINSIGHTS_KEY")
var mongoURL = os.Getenv("MONGOURL")
var amqpURL = os.Getenv("AMQPURL")
var teamName = os.Getenv("TEAMNAME")

// MongoDB variables
var mongoDBSessionCopy *mgo.Session
var mongoDBSession *mgo.Session
var mongoDBCollection *mgo.Collection
var mongoDBSessionError error

// MongoDB database and collection names
var mongoDatabaseName = "k8orders"
var mongoCollectionName = "orders"
var mongoCollectionShardKey = "product"

// AMQP 0.9.1 variables
var amqp091Client amqp091.Connection
var amqp091Channel amqp091.Channel
var amqp091Queue amqp091.Queue

// AMQP 1.0 variables
var amqp10Client amqp10.Client
var amqp10Session *amqp10.Session
var eventHubName string

// Application Insights telemetry clients
var challengeTelemetryClient appinsights.TelemetryClient
var customTelemetryClient appinsights.TelemetryClient

// For tracking and code branching purposes
var isCosmosDb = strings.Contains(mongoURL, "documents.azure.com")
var isEventHub = strings.Contains(amqpURL, "servicebus.windows.net")
var db string // CosmosDB or MongoDB?
var queueType string // EventHub or RabbitMQ

// AddOrderToMongoDB Adds the order to MongoDB/CosmosDB
func AddOrderToMongoDB(order Order) Order {
	success := false
	startTime := time.Now()

	// Use the existing mongoDBSessionCopy
	mongoDBSessionCopy = mongoDBSession.Copy()

	log.Println("Team " + teamName)

	// Select a random partition
	rand.Seed(time.Now().UnixNano())
	partitionKey := strconv.Itoa(random(0, 11))
	order.Product = fmt.Sprintf("product-%s", partitionKey)

	NewOrderID := bson.NewObjectId()
	order.ID = NewOrderID.Hex()

	order.Status = "Open"
	if order.Source == "" || order.Source == "string" {
		order.Source = os.Getenv("SOURCE")
	}

	log.Print("Mongo URL: ", mongoURL, " CosmosDB: ", isCosmosDb)

	defer mongoDBSessionCopy.Close()

	// insert Document in collection
	mongoDBSessionError = mongoDBCollection.Insert(order)
	log.Println("_id:", order)

	if mongoDBSessionError != nil {
		log.Fatal("Problem inserting data: ", mongoDBSessionError)
		log.Println("_id:", order)

		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(mongoDBSessionError)
		}
	} else {
		success = true
	}

	// Track the event for the challenge purposes
	challengeTelemetryClient.TrackEvent("CapureOrder: - Team Name " + teamName + " db " + db)

	endTime := time.Now()

	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		if isCosmosDb {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"CosmosDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Insert order"			
			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)	
		} else {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"MongoDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Insert order"	
			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)		
		}
	}

	return order
}

// AddOrderToAMQP Adds the order to AMQP (EventHub/RabbitMQ)
func AddOrderToAMQP(order Order) {
	if isEventHub {
		addOrderToAMQP10(order)
	} else {
		addOrderToAMQP091(order)
	}
}

//// BEGIN: NON EXPORTED FUNCTIONS
func init() {

	// Validate environment variables
	validateVariable(customInsightsKey, "APPINSIGHTS_KEY")
	validateVariable(challengeInsightsKey, "CHALLENGEAPPINSIGHTS_KEY")
	validateVariable(mongoURL, "MONGOURL")
	validateVariable(amqpURL, "AMQPURL")
	validateVariable(teamName, "TEAMNAME")

	// Initialize the Application Insights telemtry client(s)
	challengeTelemetryClient = appinsights.NewTelemetryClient(challengeInsightsKey)
	if customInsightsKey != "" {
		customTelemetryClient = appinsights.NewTelemetryClient(customInsightsKey)
	}

	// Initialize the MongoDB client
	initMongo()

	// Initialize the AMQP client
	initAMQP()
}

// Logs out value of a variable
func validateVariable(value string, envName string) {
	if len(value) == 0 {
		log.Printf("The environment variable %s has not been set", envName)
	} else {
		log.Printf("The environment variable %s is %s", envName, value)
	}
}

// Initialize the MongoDB client
func initMongo() {
	url, err := url.Parse(mongoURL)
	if err != nil {
		log.Fatal(fmt.Sprintf("Problem parsing Mongo URL %s: ",url), err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}

	if isCosmosDb {
		log.Println("Using CosmosDB")
		db = "CosmosDB"

	} else {
		log.Println("Using MongoDB")
		db = "MongoDB"
	}

	// Parse the connection string to extract components because the MongoDB driver is peculiar
	var dialInfo *mgo.DialInfo
	mongoUsername := ""
	mongoPassword := ""
	if url.User!=nil {
		mongoUsername = url.User.Username()
		mongoPassword, _ = url.User.Password()
	}
	mongoHost := url.Host
	mongoDatabase := "db" // can be anything
	mongoSSL := strings.Contains(url.RawQuery, "ssl=true")

	log.Printf("\tUsername: %s", mongoUsername)
	log.Printf("\tPassword: %s", mongoPassword)
	log.Printf("\tHost: %s", mongoHost)
	log.Printf("\tDatabase: %s", mongoDatabase)
	log.Printf("\tSSL: %t", mongoSSL)

	if mongoSSL {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  60 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
			DialServer: func(addr *mgo.ServerAddr) (net.Conn, error) {
				return tls.Dial("tcp", addr.String(), &tls.Config{})
			},
		}
	} else {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  60 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
		}
	}

	// Create a mongoDBSession which maintains a pool of socket connections
	// to our MongoDB.
	success := false
	startTime := time.Now()

	mongoDBSession, mongoDBSessionError = mgo.DialWithInfo(dialInfo)
	if mongoDBSessionError != nil {
		log.Fatal(fmt.Sprintf("Can't connect to mongo at [%s], go error: ", mongoURL), mongoDBSessionError)
	} else {
		success = true
	}

	endTime := time.Now()
	
	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		if isCosmosDb {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"CosmosDB",
				"MongoDB",
				mongoURL,
				success)		
				dependency.Data = "Create session"			
			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.TrackException(mongoDBSessionError)
			customTelemetryClient.Track(dependency)
		} else {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"MongoDB",
				"MongoDB",
				mongoURL,
				success)		
				dependency.Data = "Create session"			
			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.TrackException(mongoDBSessionError)
			customTelemetryClient.Track(dependency)
		}
	}
	if !success {
		os.Exit(1)
	}
	
	mongoDBSessionCopy = mongoDBSession.Copy()
		

	// SetSafe changes the mongoDBSessionCopy safety mode.
	// If the safe parameter is nil, the mongoDBSessionCopy is put in unsafe mode, and writes become fire-and-forget,
	// without error checking. The unsafe mode is faster since operations won't hold on waiting for a confirmation.
	// http://godoc.org/labix.org/v2/mgo#Session.SetMode.
	mongoDBSessionCopy.SetSafe(nil)

	// Create a sharded collection and retrieve it
	result := bson.M{}
	err = mongoDBSessionCopy.DB(mongoDatabaseName).Run(
		bson.D{
			{
				"shardCollection",
				fmt.Sprintf("%s.%s",mongoDatabaseName,mongoCollectionName),
			},
			{
				"key",
				bson.M{
					mongoCollectionShardKey: "hashed",
				},
			},
		}, &result);

	if err != nil {
		// The collection is most likely created and already sharded. I couldn't find a more elegant way to check this.
		log.Println("Could not create/re-create sharded MongoDB collection. Either collection is already sharded or sharding is not supported: ", err)
	} else {
		log.Println("Created MongoDB collection: ")
		log.Println(result)
	}

	// Get collection
	mongoDBCollection = mongoDBSessionCopy.DB(mongoDatabaseName).C(mongoCollectionName)
}

// Initalize AMQP by figuring out where we are running
func initAMQP() {
	url, err := url.Parse(amqpURL)
	if err != nil {
		log.Fatal(fmt.Sprintf("Problem parsing AMQP Host %s: ",url), err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}

	// Figure out if we're running on EventHub or elsewhere
	if isEventHub {
		log.Println("Using EventHub")
		queueType = "EventHub"

		// Parse the eventHubName (last part of the url)
		eventHubName = url.Path
	} else {
		log.Println("Using RabbitMQ")
		queueType = "RabbitMQ"
	}
	log.Println("\tAMQP URL: " + amqpURL)
}

// addOrderToAMQP091 Adds the order to AMQP 0.9.1
func addOrderToAMQP091(order Order) {
	success := false
	startTime := time.Now()
	body := fmt.Sprintf("{{'order': '%s', 'source': '%s'}}", order.ID, teamName)

	amqp091Client, err := amqp091.Dial(amqpURL)
	if err != nil {
		log.Fatal("Creating client: ", err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}

	amqp091Channel, err := amqp091Client.Channel()
	if err != nil {
		log.Fatal("Creating channel: ", err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}

	amqp091Queue, err := amqp091Channel.QueueDeclare(
		"order", // name
		true,    // durable
		false,   // delete when unused
		false,   // exclusive
		false,   // no-wait
		nil,     // arguments
	)

	// Send message
	err = amqp091Channel.Publish(
		"",     // exchange
		amqp091Queue.Name, // routing key
		false,  // mandatory
		false,  // immediate
		amqp091.Publishing{
			DeliveryMode: amqp091.Persistent,
			ContentType:  "application/json",
			Body:         []byte(body),
		})
	if err != nil {
		log.Fatal("Sending message:", err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	} else {
		success = true
	}

	endTime := time.Now()

	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		dependency := appinsights.NewRemoteDependencyTelemetry(
			"RabbitMQ",
			"AMQP",
			amqpURL,
			success)		
			dependency.Data = "Send message"			
		dependency.MarkTime(startTime, endTime)
		customTelemetryClient.Track(dependency)
	}

	log.Printf("Sent to AMQP 0.9.1 (RabbitMQ) - %t, %s: %s", success, amqpURL, body)
}

// addOrderToAMQP10 Adds the order to AMQP 1.0 (sends to the Default ConsumerGroup)
func addOrderToAMQP10(order Order) {
	success := false
	startTime := time.Now()
	body := fmt.Sprintf("{{'order': '%s', 'source': '%s'}}", order.ID, teamName)

	amqp10Client, err := amqp10.Dial(amqpURL)
	if err != nil {
		log.Fatal("Creating client: ", err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}
	defer amqp10Client.Close()

	// Send to AMQP
	amqp10Session, err := amqp10Client.NewSession()	
	if err != nil {
		log.Fatal("Creating session: ", err)
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}


	// Get a context
	amqp10Context := context.Background()
	{
		// Include a random partition
		rand.Seed(time.Now().UnixNano())
		partitionKey := strconv.Itoa(random(0, 3))
		targetAddress := fmt.Sprintf("%s/partitions/%s", eventHubName, partitionKey)

		log.Printf("AMQP URL: %s, Target: %s", amqpURL, targetAddress)

		sender, err := amqp10Session.NewSender(
			amqp10.LinkTargetAddress(targetAddress),
		)
		if err != nil {
			log.Fatal("Creating sender link: ", err)
			// If the team provided an Application Insights key, let's track that exception
			if customTelemetryClient != nil {
				customTelemetryClient.TrackException(err)
			}
		}

		amqp10Context, cancel := context.WithTimeout(amqp10Context, 5*time.Second)

		// Send message
		err = sender.Send(amqp10Context, amqp10.NewMessage([]byte(body)))
		if err != nil {
			log.Fatal("Sending message:", err)
			// If the team provided an Application Insights key, let's track that exception
			if customTelemetryClient != nil {
				customTelemetryClient.TrackException(err)
			}
		} else {
			success = true
		}


		cancel()
		sender.Close()
	}

	endTime := time.Now()

	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		dependency := appinsights.NewRemoteDependencyTelemetry(
			"EventHub",
			"AMQP",
			amqpURL,
			success)
		dependency.Data = "Send message"		
		dependency.MarkTime(startTime, endTime)
		customTelemetryClient.Track(dependency)
	}

	log.Printf("Sent to AMQP 1.0 (EventHub) - %t, %s: %s", success, amqpURL, body)
}

// random: Generates a random number
func random(min int, max int) int {
	return rand.Intn(max-min) + min
}

//// END: NON EXPORTED FUNCTIONS