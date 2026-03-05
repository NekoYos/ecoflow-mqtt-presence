package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const appVersion = "0.1.1"

type responseEnvelope struct {
	Code interface{}            `json:"code"`
	Data map[string]interface{} `json:"data"`
}

type config struct {
	APIHost     string
	AccessKey   string
	SecretKey   string
	ClientID    string
	Keepalive   int
	QoS         byte
	Quiet       bool
	ShowVersion bool
}

func hmacSHA256Hex(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func signedGet(apiHost, path, accessKey, secretKey string, params map[string]string) (map[string]interface{}, error) {
	nonce := strconv.Itoa(rand.Intn(990001) + 10000)
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signString := fmt.Sprintf("accessKey=%s&nonce=%s&timestamp=%s", accessKey, nonce, ts)
	sign := hmacSHA256Hex(secretKey, signString)

	u := url.URL{Scheme: "https", Host: apiHost, Path: path}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accessKey", accessKey)
	req.Header.Set("nonce", nonce)
	req.Header.Set("timestamp", ts)
	req.Header.Set("sign", sign)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var env responseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if fmt.Sprint(env.Code) != "0" {
		raw, _ := json.Marshal(env)
		return nil, fmt.Errorf("ecoflow error: %s", raw)
	}
	return env.Data, nil
}

func getMQTTCredentials(apiHost, accessKey, secretKey string) (map[string]interface{}, error) {
	return signedGet(apiHost, "/iot-open/sign/certification", accessKey, secretKey, nil)
}

func envInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", name, raw)
	}
	return v, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true/false, got %q", name, raw)
	}
	return v, nil
}

func parseFlags() (*config, error) {
	cfg := &config{}
	var qos int

	defaultKeepalive, err := envInt("ECOFLOW_KEEPALIVE", 30)
	if err != nil {
		return nil, err
	}
	defaultQos, err := envInt("ECOFLOW_QOS", 0)
	if err != nil {
		return nil, err
	}
	defaultQuiet, err := envBool("ECOFLOW_QUIET", false)
	if err != nil {
		return nil, err
	}

	flag.StringVar(&cfg.APIHost, "api-host", os.Getenv("ECOFLOW_API_HOST"), "usually api-e.ecoflow.com or api-a.ecoflow.com")
	flag.StringVar(&cfg.AccessKey, "access-key", os.Getenv("ECOFLOW_ACCESS_KEY"), "EcoFlow access key")
	flag.StringVar(&cfg.SecretKey, "secret-key", os.Getenv("ECOFLOW_SECRET_KEY"), "EcoFlow secret key")
	flag.StringVar(&cfg.ClientID, "client-id", os.Getenv("ECOFLOW_CLIENT_ID"), "STATIC client_id")
	flag.IntVar(&cfg.Keepalive, "keepalive", defaultKeepalive, "MQTT keepalive")
	flag.IntVar(&qos, "qos", defaultQos, "MQTT qos (0,1,2)")
	flag.BoolVar(&cfg.Quiet, "quiet", defaultQuiet, "do not print incoming messages at all")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "print application version and exit")
	flag.Parse()

	if cfg.ShowVersion {
		return cfg, nil
	}

	if cfg.APIHost == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("--api-host, --access-key and --secret-key are required (or set ECOFLOW_API_HOST, ECOFLOW_ACCESS_KEY, ECOFLOW_SECRET_KEY)")
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "ecoflow_presence_static"
	}
	if qos < 0 || qos > 2 {
		return nil, fmt.Errorf("--qos must be 0, 1, or 2")
	}
	cfg.QoS = byte(qos)

	return cfg, nil
}

func main() {
	rand.Seed(time.Now().UnixNano())

	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		flag.Usage()
		os.Exit(2)
	}
	if cfg.ShowVersion {
		fmt.Println(appVersion)
		return
	}

	creds, err := getMQTTCredentials(cfg.APIHost, cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to get MQTT credentials:", err)
		os.Exit(1)
	}

	mqttUser, _ := creds["certificateAccount"].(string)
	mqttPass, _ := creds["certificatePassword"].(string)
	broker, _ := creds["url"].(string)
	portRaw := fmt.Sprint(creds["port"])
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid MQTT port:", portRaw)
		os.Exit(1)
	}

	quotaWild := fmt.Sprintf("/open/%s/+/quota", mqttUser)
	statusWild := fmt.Sprintf("/open/%s/+/status", mqttUser)
	setReplyWild := fmt.Sprintf("/open/%s/+/set_reply", mqttUser)

	fmt.Println("MQTT broker:", broker, port)
	fmt.Println("MQTT user:", mqttUser)
	fmt.Println("Subscribing to:")
	fmt.Println(" ", quotaWild)
	fmt.Println(" ", statusWild)
	fmt.Println(" ", setReplyWild)

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("ssl://%s:%d", broker, port))
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(mqttUser)
	opts.SetPassword(mqttPass)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(time.Duration(cfg.Keepalive) * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(1 * time.Second)
	opts.SetMaxReconnectInterval(60 * time.Second)
	opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})

	opts.OnConnect = func(c mqtt.Client) {
		fmt.Println("Connected")
		topicMap := map[string]byte{
			quotaWild:    cfg.QoS,
			statusWild:   cfg.QoS,
			setReplyWild: cfg.QoS,
		}
		t := c.SubscribeMultiple(topicMap, nil)
		t.Wait()
		if t.Error() != nil {
			fmt.Println("Subscribe error:", t.Error())
			return
		}
		fmt.Println("Subscribed. Presence mode is ON.")
	}

	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		fmt.Println("Disconnected:", err, "(will reconnect automatically)")
	}

	opts.SetDefaultPublishHandler(func(_ mqtt.Client, msg mqtt.Message) {
		if cfg.Quiet {
			return
		}
		text := string(msg.Payload())
		if len(text) > 500 {
			text = text[:500]
		}
		fmt.Printf("[%s] %s\n", msg.Topic(), text)
	})

	client := mqtt.NewClient(opts)
	ct := client.Connect()
	ct.Wait()
	if ct.Error() != nil {
		fmt.Fprintln(os.Stderr, "MQTT connect failed:", ct.Error())
		os.Exit(1)
	}

	select {}
}
