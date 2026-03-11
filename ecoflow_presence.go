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
	"sort"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const appVersion = "0.3.1"

const (
	healthcheckDisabled  = "false"
	healthcheckSmartPlug = "smartplug"
)

type responseEnvelope struct {
	Code interface{}     `json:"code"`
	Data json.RawMessage `json:"data"`
}

type mqttCredentials struct {
	CertificateAccount  string      `json:"certificateAccount"`
	CertificatePassword string      `json:"certificatePassword"`
	URL                 string      `json:"url"`
	Port                interface{} `json:"port"`
}

type deviceInfo struct {
	SN          string `json:"sn"`
	DeviceName  string `json:"deviceName"`
	ProductName string `json:"productName"`
}

type config struct {
	APIHost                     string
	AccessKey                   string
	SecretKey                   string
	ClientID                    string
	Keepalive                   int
	QoS                         byte
	Quiet                       bool
	ShowVersion                 bool
	HealthcheckType             string
	HealthcheckInterval         time.Duration
	HealthcheckPlugHeartbeatMax float64
}

func hmacSHA256Hex(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func buildSignString(accessKey, nonce, timestamp string, params map[string]string) string {
	parts := make([]string, 0, len(params)+3)

	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for key := range params {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key+"="+params[key])
		}
	}

	parts = append(parts,
		"accessKey="+accessKey,
		"nonce="+nonce,
		"timestamp="+timestamp,
	)

	return strings.Join(parts, "&")
}

func signedGetRaw(apiHost, path, accessKey, secretKey string, params map[string]string) (json.RawMessage, error) {
	nonce := strconv.Itoa(rand.Intn(990001) + 10000)
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signString := buildSignString(accessKey, nonce, ts, params)
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

func signedGetJSON(apiHost, path, accessKey, secretKey string, params map[string]string, dst interface{}) error {
	raw, err := signedGetRaw(apiHost, path, accessKey, secretKey, params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

func getMQTTCredentials(apiHost, accessKey, secretKey string) (*mqttCredentials, error) {
	var creds mqttCredentials
	if err := signedGetJSON(apiHost, "/iot-open/sign/certification", accessKey, secretKey, nil, &creds); err != nil {
		return nil, err
	}
	return &creds, nil
}

func getDeviceList(apiHost, accessKey, secretKey string) ([]deviceInfo, error) {
	var devices []deviceInfo
	if err := signedGetJSON(apiHost, "/iot-open/sign/device/list", accessKey, secretKey, nil, &devices); err != nil {
		return nil, err
	}
	return devices, nil
}

func getDeviceQuotaAll(apiHost, accessKey, secretKey, sn string) (map[string]interface{}, error) {
	var payload map[string]interface{}
	if err := signedGetJSON(apiHost, "/iot-open/sign/device/quota/all", accessKey, secretKey, map[string]string{"sn": sn}, &payload); err != nil {
		return nil, err
	}
	return payload, nil
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

func envFloat(name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
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

func envDurationSeconds(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number of seconds, got %q", name, raw)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return time.Duration(seconds) * time.Second, nil
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
	defaultHealthInterval, err := envDurationSeconds("ECOFLOW_HEALTHCHECK_INTERVAL", 60*time.Second)
	if err != nil {
		return nil, err
	}
	defaultPlugHeartbeatMax, err := envFloat("ECOFLOW_HEALTHCHECK_SMARTPLUG_MAX_HEARTBEAT", 900)
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
	flag.StringVar(&cfg.HealthcheckType, "healthcheck-type", envOrDefault("ECOFLOW_HEALTHCHECK_TYPE", healthcheckDisabled), "false, smartplug")
	flag.DurationVar(&cfg.HealthcheckInterval, "healthcheck-interval", defaultHealthInterval, "healthcheck polling interval")
	flag.Float64Var(&cfg.HealthcheckPlugHeartbeatMax, "healthcheck-smartplug-max-heartbeat", defaultPlugHeartbeatMax, "max acceptable smart plug 2_1.heartbeatFrequency value")
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

	cfg.HealthcheckType = strings.ToLower(strings.TrimSpace(cfg.HealthcheckType))
	switch cfg.HealthcheckType {
	case "", healthcheckDisabled:
		cfg.HealthcheckType = healthcheckDisabled
	case healthcheckSmartPlug:
	default:
		return nil, fmt.Errorf("unsupported healthcheck type %q, expected false or smartplug", cfg.HealthcheckType)
	}

	if cfg.HealthcheckInterval <= 0 {
		return nil, fmt.Errorf("--healthcheck-interval must be greater than zero")
	}
	if cfg.HealthcheckPlugHeartbeatMax <= 0 {
		return nil, fmt.Errorf("--healthcheck-smartplug-max-heartbeat must be greater than zero")
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	return raw
}

func requestReconnect(ch chan<- string, reason string) {
	select {
	case ch <- reason:
	default:
	}
}

func formatDeviceLabel(device deviceInfo) string {
	if device.DeviceName != "" {
		return fmt.Sprintf("%s (%s)", device.DeviceName, device.SN)
	}
	if device.ProductName != "" {
		return fmt.Sprintf("%s (%s)", device.ProductName, device.SN)
	}
	return device.SN
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func selectSmartPlugs(devices []deviceInfo) []deviceInfo {
	selected := make([]deviceInfo, 0)
	for _, device := range devices {
		if containsFold(device.ProductName, "plug") || containsFold(device.DeviceName, "plug") {
			selected = append(selected, device)
		}
	}
	return selected
}

func extractByPath(value interface{}, path []string) (interface{}, bool) {
	if len(path) == 0 {
		return value, true
	}

	switch typed := value.(type) {
	case map[string]interface{}:
		next, ok := typed[path[0]]
		if !ok {
			return nil, false
		}
		return extractByPath(next, path[1:])
	case []interface{}:
		index, err := strconv.Atoi(path[0])
		if err != nil || index < 0 || index >= len(typed) {
			return nil, false
		}
		return extractByPath(typed[index], path[1:])
	default:
		return nil, false
	}
}

func findAny(value interface{}, names ...string) (interface{}, bool) {
	if len(names) == 0 {
		return nil, false
	}

	if current, ok := value.(map[string]interface{}); ok {
		for _, name := range names {
			if found, exists := current[name]; exists {
				return found, true
			}
		}
		for _, nested := range current {
			if found, ok := findAny(nested, names...); ok {
				return found, true
			}
		}
		return nil, false
	}

	if current, ok := value.([]interface{}); ok {
		for _, nested := range current {
			if found, ok := findAny(nested, names...); ok {
				return found, true
			}
		}
	}

	return nil, false
}

func numericValue(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func stringifyValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

func extractMetricValue(payload map[string]interface{}, metric string, aliases ...string) (interface{}, bool) {
	candidates := append([]string{metric}, aliases...)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, ".") {
			if value, ok := extractByPath(payload, strings.Split(candidate, ".")); ok {
				return value, true
			}
		}
		if value, ok := payload[candidate]; ok {
			return value, true
		}
		if value, ok := findAny(payload, candidate); ok {
			return value, true
		}
	}
	return nil, false
}

func runSmartPlugHealthcheck(cfg *config) (string, error) {
	devices, err := getDeviceList(cfg.APIHost, cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		return "", err
	}

	plugs := selectSmartPlugs(devices)
	if len(plugs) == 0 {
		return "", fmt.Errorf("smartplug healthcheck enabled, but no smart plugs found")
	}

	for _, device := range plugs {
		payload, err := getDeviceQuotaAll(cfg.APIHost, cfg.AccessKey, cfg.SecretKey, device.SN)
		if err != nil {
			return "", fmt.Errorf("%s quota/all failed: %w", formatDeviceLabel(device), err)
		}

		value, ok := extractMetricValue(payload, "2_1.heartbeatFrequency")
		if !ok {
			return "", fmt.Errorf("%s does not expose 2_1.heartbeatFrequency", formatDeviceLabel(device))
		}

		heartbeat, ok := numericValue(value)
		if !ok {
			return "", fmt.Errorf("%s 2_1.heartbeatFrequency is not numeric: %s", formatDeviceLabel(device), stringifyValue(value))
		}

		fmt.Printf(
			"Healthcheck smartplug %s 2_1.heartbeatFrequency=%.0f threshold=%.0f\n",
			formatDeviceLabel(device),
			heartbeat,
			cfg.HealthcheckPlugHeartbeatMax,
		)
		if heartbeat >= cfg.HealthcheckPlugHeartbeatMax {
			return fmt.Sprintf("%s 2_1.heartbeatFrequency %.0f >= %.0f", formatDeviceLabel(device), heartbeat, cfg.HealthcheckPlugHeartbeatMax), nil
		}
	}

	fmt.Printf("Healthcheck smartplug OK: checked %d plug(s), all heartbeat values below threshold\n", len(plugs))
	return "", nil
}

func startHealthcheckLoop(cfg *config, reconnectCh chan<- string) {
	if cfg.HealthcheckType == healthcheckDisabled {
		fmt.Println("Healthcheck disabled")
		return
	}

	fmt.Printf(
		"Healthcheck enabled: type=%s interval=%s\n",
		cfg.HealthcheckType,
		cfg.HealthcheckInterval,
	)

	ticker := time.NewTicker(cfg.HealthcheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Printf("Healthcheck tick: type=%s interval=%s\n", cfg.HealthcheckType, cfg.HealthcheckInterval)
		reason, err := runSmartPlugHealthcheck(cfg)
		if err != nil {
			fmt.Println("Healthcheck error:", err)
			continue
		}
		if reason == "" {
			continue
		}

		fmt.Println("Healthcheck requested reconnect:", reason)
		requestReconnect(reconnectCh, reason)
	}
}

func runSession(cfg *config, reconnectCh <-chan string) string {
	fmt.Println("Requesting fresh MQTT credentials")
	creds, err := getMQTTCredentials(cfg.APIHost, cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to get MQTT credentials:", err)
		return "credential refresh failed"
	}

	portRaw := fmt.Sprint(creds.Port)
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid MQTT port:", portRaw)
		return "invalid MQTT port"
	}

	mqttUser := creds.CertificateAccount
	quotaWild := fmt.Sprintf("/open/%s/+/quota", mqttUser)
	statusWild := fmt.Sprintf("/open/%s/+/status", mqttUser)
	setReplyWild := fmt.Sprintf("/open/%s/+/set_reply", mqttUser)

	fmt.Println("MQTT broker:", creds.URL, port)
	fmt.Println("MQTT user:", mqttUser)
	fmt.Println("Subscribing to:")
	fmt.Println(" ", quotaWild)
	fmt.Println(" ", statusWild)
	fmt.Println(" ", setReplyWild)
	fmt.Println("Opening MQTT session")

	lostCh := make(chan error, 1)
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("ssl://%s:%d", creds.URL, port))
	opts.SetClientID(cfg.ClientID)
	opts.SetUsername(mqttUser)
	opts.SetPassword(creds.CertificatePassword)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(time.Duration(cfg.Keepalive) * time.Second)
	opts.SetAutoReconnect(false)
	opts.SetConnectRetry(false)
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
			requestReconnectAsError(lostCh, fmt.Errorf("subscribe failed: %w", t.Error()))
			return
		}

		fmt.Println("Subscribed. Presence mode is ON.")
	}

	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		fmt.Println("Disconnected:", err)
		requestReconnectAsError(lostCh, err)
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
		return "initial connect failed"
	}
	fmt.Println("MQTT session established, waiting for events")

	select {
	case err := <-lostCh:
		if err != nil {
			fmt.Fprintln(os.Stderr, "Session ended:", err)
		}
		client.Disconnect(250)
		return "connection lost"
	case reason := <-reconnectCh:
		fmt.Println("Reconnecting:", reason)
		client.Disconnect(250)
		return reason
	}
}

func requestReconnectAsError(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
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

	reconnectCh := make(chan string, 1)

	go startHealthcheckLoop(cfg, reconnectCh)

	for {
		reason := runSession(cfg, reconnectCh)
		wait := 3 * time.Second
		if strings.Contains(strings.ToLower(reason), "heartbeat") {
			wait = 1 * time.Second
		}
		fmt.Printf("Restarting MQTT session in %s, reason: %s\n", wait, reason)
		time.Sleep(wait)
	}
}
