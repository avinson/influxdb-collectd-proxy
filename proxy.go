package main

import (
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	influxdb "github.com/influxdb/influxdb-go"
	collectd "github.com/paulhammond/gocollectd"
)

const influxWriteInterval = time.Second
const influxWriteLimit = 50
const packetChannelSize = 100

var (
	proxyPort   *string
	typesdbPath *string
	logPath     *string
	verbose     *bool

	// influxdb options
	host      *string
	username  *string
	password  *string
	database  *string
	normalize *bool

	types       Types
	client      *influxdb.Client
	beforeCache map[string]CacheEntry
)

// point cache to perform data normalization for COUNTER and DERIVE types
type CacheEntry struct {
	Timestamp int64
	Value     float64
}

// signal handler
func handleSignals(c chan os.Signal) {
	// block until a signal is received
	sig := <-c

	log.Printf("exit with a signal: %v\n", sig)
	os.Exit(1)
}

func init() {
	// proxy options
	proxyPort = flag.String("proxyport", "8096", "port for proxy")
	typesdbPath = flag.String("typesdb", "types.db", "path to Collectd's types.db")
	logPath = flag.String("logfile", "proxy.log", "path to log file")
	verbose = flag.Bool("verbose", false, "true if you need to trace the requests")

	// influxdb options
	host = flag.String("influxdb", "localhost:8086", "host:port for influxdb")
	username = flag.String("username", "root", "username for influxdb")
	password = flag.String("password", "root", "password for influxdb")
	database = flag.String("database", "", "database for influxdb")
	normalize = flag.Bool("normalize", true, "true if you need to normalize data for COUNTER and DERIVE types (over time)")

	flag.Parse()

	beforeCache = make(map[string]CacheEntry)

	// read types.db
	var err error
	types, err = ParseTypesDB(*typesdbPath)
	if err != nil {
		log.Fatalf("failed to read types.db: %v\n", err)
	}
}

func main() {
	logFile, err := os.OpenFile(*logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("failed to open file: %v\n", err)
	}
	log.SetOutput(logFile)
	defer logFile.Close()

	// make influxdb client
	client, err = influxdb.NewClient(&influxdb.ClientConfig{
		Host:     *host,
		Username: *username,
		Password: *password,
		Database: *database,
	})
	if err != nil {
		log.Fatalf("failed to make a influxdb client: %v\n", err)
	}

	// register a signal handler
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt, os.Kill)
	go handleSignals(sc)

	// make channel for collectd
	c := make(chan collectd.Packet, packetChannelSize)

	// then start to listen
	go collectd.Listen("0.0.0.0:"+*proxyPort, c)
	log.Printf("proxy started on %s\n", *proxyPort)
	timer := time.Now()
	seriesGroup := make([]*influxdb.Series, 0)
	for packet := range c {
		seriesGroup = append(seriesGroup, processPacket(packet)...)

		if time.Since(timer) < influxWriteInterval && len(seriesGroup) < influxWriteLimit {
			continue
		} else {
			if len(seriesGroup) > 0 {
				go backendWriter(seriesGroup)
				seriesGroup = make([]*influxdb.Series, 0)
			}
			timer = time.Now()
		}
	}
}

func backendWriter(seriesGroup []*influxdb.Series) {
	if err := client.WriteSeries(seriesGroup); err != nil {
		log.Printf("failed to write series group to influxdb: %s\n", err)
	}
	if *verbose {
		log.Printf("[TRACE] wrote %d series\n", len(seriesGroup))
	}
}

func processPacket(packet collectd.Packet) []*influxdb.Series {
	if *verbose {
		log.Printf("[TRACE] got a packet: %v\n", packet)
	}

	var seriesGroup []*influxdb.Series
	// for all metrics in the packet
	for i, _ := range packet.ValueNames() {
		values, _ := packet.ValueNumbers()

		// get a type for this packet
		t := types[packet.Type]

		// pass the unknowns
		if t == nil && packet.TypeInstance == "" {
			log.Printf("unknown type instance on %s\n", packet.Plugin)
			continue
		}

		// as hostname contains commas, let's replace them
		hostName := strings.Replace(packet.Hostname, ".", "_", -1)

		// if there's a PluginInstance, use it
		pluginName := packet.Plugin
		if packet.PluginInstance != "" {
			pluginName += "-" + packet.PluginInstance
		}

		// if there's a TypeInstance, use it
		typeName := packet.Type
		if packet.TypeInstance != "" {
			typeName += "-" + packet.TypeInstance
		} else if t != nil {
			typeName += "-" + t[i]
		}

		cacheKey := hostName + "." + pluginName + "." + typeName
		name := pluginName + "." + typeName

		// influxdb stuffs
		timestamp := packet.Time().UnixNano() / 1000000
		value := values[i].Float64()
		dataType := packet.DataTypes[i]
		readyToSend := true
		normalizedValue := value

		if *normalize && dataType == collectd.TypeCounter || dataType == collectd.TypeDerive {
			if before, ok := beforeCache[cacheKey]; ok && !math.IsNaN(before.Value) {
				// normalize over time
				if timestamp-before.Timestamp > 0 {
					normalizedValue = (value - before.Value) / float64((timestamp-before.Timestamp)/1000)
				} else {
					normalizedValue = value - before.Value
				}
			} else {
				// skip current data if there's no initial entry
				readyToSend = false
			}
			entry := CacheEntry{
				Timestamp: timestamp,
				Value:     value,
			}
			beforeCache[cacheKey] = entry
		}

		if readyToSend {
			series := &influxdb.Series{
				Name:    name,
				Columns: []string{"time", "value", "host"},
				Points: [][]interface{}{
					[]interface{}{timestamp, normalizedValue, hostName},
				},
			}
			if *verbose {
				log.Printf("[TRACE] ready to send series: %v\n", series)
			}
			seriesGroup = append(seriesGroup, series)
		}
	}
	return seriesGroup
}
