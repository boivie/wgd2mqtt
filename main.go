package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type response struct {
	CurrentObservation struct {
		ObservationLocation struct {
			Latitude  string `json:"latitude"`
			Longitude string `json:"longitude"`
		} `json:"observation_location"`
		StationID         string  `json:"station_id"`
		TempC             float64 `json:"temp_c"`
		RelativeHumidity  string  `json:"relative_humidity"`
		WindDegrees       int32   `json:"wind_degrees"`
		WindKph           float64 `json:"wind_kph"`
		FeelsLikeC        string  `json:"feelslike_c"`
		PrecipTodayMetric string  `json:"precip_today_metric"`
	} `json:"current_observation"`
}

var temperature = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "thermometer_temperature_celsius",
		Help: "Current temperature of the thermometer.",
	},
	[]string{"sensor_name", "area"},
)

var humidity = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "hygrometer_humidity_percent",
		Help: "Current humidity of the hygrometer.",
	},
	[]string{"sensor_name", "area"},
)

var precipitation = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "precipitation_mm",
		Help: "Today's precipitation in mm.",
	},
	[]string{"sensor_name", "area"},
)

var windDirection = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "wind_direction_degrees",
		Help: "Current wind direction in degrees",
	},
	[]string{"sensor_name", "area"},
)

var windSpeed = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "wind_speed_kph",
		Help: "Current wind speed in kph",
	},
	[]string{"sensor_name", "area"},
)

const area = "wunderground"

func init() {
	prometheus.MustRegister(temperature)
	prometheus.MustRegister(humidity)
	prometheus.MustRegister(precipitation)
	prometheus.MustRegister(windDirection)
	prometheus.MustRegister(windSpeed)
}

func topic(stationID string, property string) string {
	return fmt.Sprintf("weather_underground/stations/%s/%s", stationID, property)
}

func updater(apiKey string, stationID string, client MQTT.Client) {
	t := time.NewTicker(20 * time.Minute)
	for {
		fmt.Printf("%s: Fetching latest observation\n", stationID)
		url := fmt.Sprintf("http://api.wunderground.com/api/%s/conditions/q/pws:%s.json", apiKey, stationID)

		res, err := http.Get(url)
		if err != nil || res.StatusCode != 200 {
			fmt.Printf("%s: Failed to perform HTTP GET\n", stationID)
		} else {
			d := json.NewDecoder(res.Body)
			var data response
			if err = d.Decode(&data); err != nil || data.CurrentObservation.StationID != stationID {
				fmt.Printf("%s: Failed to decode JSON: %v\n", stationID, err)
			} else {
				client.Publish(topic(stationID, "latitude"), 0, true, data.CurrentObservation.ObservationLocation.Latitude)
				client.Publish(topic(stationID, "longitude"), 0, true, data.CurrentObservation.ObservationLocation.Longitude)

				client.Publish(topic(stationID, "temperature_degrees"), 0, true, data.CurrentObservation.TempC)
				fmt.Printf("%s: %.1f C\n", stationID, data.CurrentObservation.TempC)
				temperature.WithLabelValues(stationID, area).Set(data.CurrentObservation.TempC)

				if strings.HasSuffix(data.CurrentObservation.RelativeHumidity, "%") {
					strval := data.CurrentObservation.RelativeHumidity[0 : len(data.CurrentObservation.RelativeHumidity)-1]
					if value, err := strconv.ParseFloat(strval, 64); err == nil {
						client.Publish(topic(stationID, "relative_humidity_percent"), 0, true, value)
						humidity.WithLabelValues(stationID, area).Set(value)
					}
				}

				if data.CurrentObservation.WindDegrees != -9999 {
					client.Publish(topic(stationID, "wind_degrees"), 0, true, data.CurrentObservation.WindDegrees)
					windDirection.WithLabelValues(stationID, area).Set(float64(data.CurrentObservation.WindDegrees))
				}
				client.Publish(topic(stationID, "wind_kph"), 0, true, data.CurrentObservation.WindKph)
				windSpeed.WithLabelValues(stationID, area).Set(data.CurrentObservation.WindKph)

				client.Publish(topic(stationID, "temperature_feels_like_degrees"), 0, true, data.CurrentObservation.FeelsLikeC)
				if value, err := strconv.ParseFloat(data.CurrentObservation.PrecipTodayMetric, 64); err == nil {
					client.Publish(topic(stationID, "precip_today_mm"), 0, true, value)
					precipitation.WithLabelValues(stationID, area).Set(value)
				}
			}
		}
		fmt.Printf("%s: Sleeping\n", stationID)
		<-t.C

	}
}

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("signal received, exiting")
		os.Exit(0)
	}()

	hostname, _ := os.Hostname()

	server := flag.String("server", "tcp://127.0.0.1:1883", "The full url of the MQTT server to connect to ex: tcp://127.0.0.1:1883")
	clientid := flag.String("clientid", hostname+strconv.Itoa(time.Now().Second()), "A clientid for the connection")
	username := flag.String("username", "", "A username to authenticate to the MQTT server")
	password := flag.String("password", "", "Password to match username")
	apiKey := flag.String("apikey", "", "API key")
	stations := flag.String("stations", "", "Comma separated list of stations")
	flag.Parse()

	connOpts := &MQTT.ClientOptions{
		ClientID:             *clientid,
		CleanSession:         true,
		Username:             *username,
		Password:             *password,
		MaxReconnectInterval: 1 * time.Second,
		KeepAlive:            int64(30 * time.Second),
		TLSConfig:            tls.Config{InsecureSkipVerify: true, ClientAuth: tls.NoClientCert},
	}
	connOpts.AddBroker(*server)

	client := MQTT.NewClient(connOpts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	} else {
		fmt.Printf("Connected to %s\n", *server)
	}

	for _, stationID := range strings.Split(*stations, ",") {
		go updater(*apiKey, stationID, client)
	}

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":8080", nil))
}
