package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version information
var (
	version   = "0.1.1"
	commit    = "unknown"
	buildDate = "unknown"
)

// TerminalResponse represents the JSON structure returned by the API
type TerminalResponse struct {
	ImageURL    string `json:"image_url"`
	Filename    string `json:"filename"`
	RefreshRate int    `json:"refresh_rate"`
}

// Config holds application configuration
type Config struct {
	APIKey   string `json:"api_key,omitempty"`   // API key for trmnl.app
	DeviceID string `json:"device_id,omitempty"` // Device ID (MAC address) for Terminus/BYOS servers
	BaseURL  string `json:"base_url,omitempty"`
}

// AppOptions holds command line options
type AppOptions struct {
	DarkMode bool
	Verbose  bool
	BaseURL  string
}

type inputEvent int

const (
	inputNext inputEvent = iota
	inputStatus
)

const (
	inkyButtonA  = 5
	inkyButtonB  = 6
	statusWidth  = 800
	statusHeight = 480
)

//  exec.Command("sudo", "service", "gpm", "stop").Run()

func main() {
	// Parse command line arguments
	options := parseCommandLineArgs()

	// Set up signal handling for clean exit
	setupSignalHandling()

	// Check the environment first
	if options.Verbose {
		fmt.Println("Checking system environment...")
		if options.DarkMode {
			fmt.Println("Dark mode enabled - images will be inverted")
		}
	}

	var err error

	// Create a configuration directory as per XDG standard:
	// at user-specified location when the environment variable is set,
	// at $HOME/.config/trmnl (XDG default config location for Unix) if not set
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Printf("Error getting home directory: %v\n", err)
			os.Exit(1)
		}
		configHome = filepath.Join(homeDir, ".config")
	}
	configDir := filepath.Join(configHome, "trmnl")
	err = os.MkdirAll(configDir, 0755)
	if err != nil {
		fmt.Printf("Error creating config directory: %v\n", err)
		os.Exit(1)
	}

	// Get configuration from file
	config := loadConfig(configDir)

	// Override with environment variables if present
	if envAPIKey := os.Getenv("TRMNL_API_KEY"); envAPIKey != "" {
		config.APIKey = envAPIKey
	}
	if envDeviceID := os.Getenv("TRMNL_DEVICE_ID"); envDeviceID != "" {
		config.DeviceID = envDeviceID
	}
	if envBaseURL := os.Getenv("TRMNL_BASE_URL"); envBaseURL != "" {
		config.BaseURL = envBaseURL
	}

	// Override with command line argument if provided
	if options.BaseURL != "" {
		config.BaseURL = options.BaseURL
	}

	// Set default base URL if not configured
	if config.BaseURL == "" {
		config.BaseURL = "https://trmnl.app"
	}

	if options.Verbose {
		fmt.Printf("Using base URL: %s\n", config.BaseURL)
	}

	// Check if we're using trmnl.app or a custom server
	isTerminusServer := !strings.Contains(config.BaseURL, "trmnl.app")

	// Ensure we have the appropriate credentials
	if isTerminusServer {
		// For Terminus/BYOS servers, we need a device ID (MAC address)
		if config.DeviceID == "" {
			// Check if API key looks like a MAC address and migrate it
			if config.APIKey != "" && strings.Count(config.APIKey, ":") == 5 {
				config.DeviceID = config.APIKey
				config.APIKey = "" // Clear API key since it's actually a device ID
			} else {
				fmt.Println("Device ID (MAC address) not found.")
				fmt.Print("Please enter your device MAC address (e.g., AA:BB:CC:DD:EE:FF): ")
				fmt.Scanln(&config.DeviceID)
			}
			saveConfig(configDir, config)
		}
	} else {
		// For trmnl.app, we need an API key
		if config.APIKey == "" {
			fmt.Println("TRMNL (device) API Key not found.")
			fmt.Println("(in the Device Credentials section of the web portal)")
			fmt.Print("Please enter your key: ")
			fmt.Scanln(&config.APIKey)
			saveConfig(configDir, config)
		}
	}

	// Create a temporary directory for storing images
	tmpDir, err := os.MkdirTemp("", "trmnl-display")
	if err != nil {
		fmt.Printf("Error creating temp directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)
	events := startInputHandlers(options)
	frames := 0
	for {
		processNextImage(tmpDir, config, options, frames, events)
		frames = frames + 1
	}
}

// setupSignalHandling sets up handlers for SIGINT, SIGTERM, and SIGHUP
func setupSignalHandling() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-c
		fmt.Println("\nReceived termination signal. Cleaning up...")
		os.Exit(0)
	}()
}

// parseCommandLineArgs parses command line arguments and returns app options
func parseCommandLineArgs() AppOptions {
	darkMode := flag.Bool("d", false, "Enable dark mode (invert image pixels)")
	showVersion := flag.Bool("v", false, "Show version information")
	verbose := flag.Bool("verbose", true, "Enable verbose output")
	quiet := flag.Bool("q", false, "Quiet mode (disable verbose output)")
	baseURL := flag.String("base-url", "", "Custom base URL for the TRMNL API (default: https://trmnl.app)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("trmnl-display version %s (commit: %s, built: %s)\n",
			version, commit, buildDate)
		os.Exit(0)
	}

	return AppOptions{
		DarkMode: *darkMode,
		Verbose:  *verbose && !*quiet,
		BaseURL:  *baseURL,
	}
}

func processNextImage(tmpDir string, config Config, options AppOptions, frames int, events <-chan inputEvent) {
	// Use defer and recover to handle any panics
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Recovered from panic: %v\n", r)
			time.Sleep(60 * time.Second)
		}
	}()

	// Get the TRMNL display
	apiURL := strings.TrimRight(config.BaseURL, "/") + "/api/display"
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}

	// Use different header based on server type
	// For Terminus servers, use MAC address in ID header
	// For standard TRMNL servers, use access-token
	if strings.Contains(config.BaseURL, "trmnl.app") {
		req.Header.Add("access-token", config.APIKey)
	} else {
		// For Terminus/BYOS servers, use ID header with MAC address
		req.Header.Add("ID", config.DeviceID)
		// Also add access-token for BYOS Laravel compatibility
		if config.APIKey != "" {
			req.Header.Add("access-token", config.APIKey)
		}
		req.Header.Add("Content-Type", "application/json")
	}
	req.Header.Add("battery-voltage", "100.00")
	req.Header.Add("rssi", "0")
	req.Header.Add("User-Agent", fmt.Sprintf("trmnl-display/%s", version))
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error fetching display: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("Error fetching display from %s: status code %d\n", apiURL, resp.StatusCode)
		if options.Verbose && resp.StatusCode == 404 {
			fmt.Printf("API endpoint not found. Please verify the base URL is correct.\n")
		}
		time.Sleep(60 * time.Second)
		return
	}

	// Parse the JSON response
	var terminal TerminalResponse
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&terminal); err != nil {
		fmt.Printf("Error parsing JSON: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}

	// Set default filename if not provided
	filename := terminal.Filename
	if filename == "" {
		filename = "display.jpg"
	}

	// Create full path to temporary file
	filePath := filepath.Join(tmpDir, filename)

	// Download the image
	imgResp, err := http.Get(terminal.ImageURL)
	if err != nil {
		fmt.Printf("Error downloading image: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}
	defer imgResp.Body.Close()

	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		fmt.Printf("Error creating file: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}

	// Copy the image data to the file
	_, err = io.Copy(out, imgResp.Body)
	if err != nil {
		fmt.Printf("Error saving image: %v\n", err)
		out.Close()
		time.Sleep(60 * time.Second)
		return
	}
	out.Close()

	// Display the image
	err = displayImage(filePath, options, frames)
	if err != nil {
		fmt.Printf("Error displaying image: %v\n", err)
		time.Sleep(60 * time.Second)
		return
	}

	// Set default refresh rate if not provided
	refreshRate := terminal.RefreshRate
	if refreshRate <= 0 {
		refreshRate = 60
	}

	waitForNextUpdate(tmpDir, options, frames, refreshRate, events)
}

func startInputHandlers(options AppOptions) <-chan inputEvent {
	events := make(chan inputEvent, 4)
	go watchKeyboard(events)
	go watchInkyButtons(events, options)
	return events
}

func sendInputEvent(events chan<- inputEvent, event inputEvent) {
	select {
	case events <- event:
	default:
	}
}

func watchKeyboard(events chan<- inputEvent) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println("Keypress...skipping to next update")
		sendInputEvent(events, inputNext)
	}
}

func watchInkyButtons(events chan<- inputEvent, options AppOptions) {
	buttons := map[int]inputEvent{
		inkyButtonA: inputNext,
		inkyButtonB: inputStatus,
	}
	reader, err := newGPIOButtonReader(buttons)
	if err != nil {
		if options.Verbose {
			fmt.Printf("Pimoroni button GPIO unavailable: %v\n", err)
		}
		return
	}

	previous := make(map[int]string, len(buttons))
	for pin := range buttons {
		value, err := reader.read(pin)
		if err != nil {
			if options.Verbose {
				fmt.Printf("Button GPIO %d read failed: %v\n", pin, err)
			}
			return
		}
		previous[pin] = value
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		for pin, event := range buttons {
			value, err := reader.read(pin)
			if err != nil {
				continue
			}
			if previous[pin] == "1" && value == "0" {
				if event == inputNext {
					fmt.Println("Button A...skipping to next update")
				} else if event == inputStatus {
					fmt.Println("Button B...showing status screen")
				}
				sendInputEvent(events, event)
			}
			previous[pin] = value
		}
	}
}

type gpioButtonReader struct {
	chip string
}

func newGPIOButtonReader(buttons map[int]inputEvent) (gpioButtonReader, error) {
	if _, err := exec.LookPath("gpioget"); err != nil {
		return gpioButtonReader{}, err
	}

	chips, err := filepath.Glob("/dev/gpiochip*")
	if err != nil {
		return gpioButtonReader{}, err
	}
	if len(chips) == 0 {
		return gpioButtonReader{}, fmt.Errorf("no /dev/gpiochip devices found")
	}

	for _, chipPath := range chips {
		reader := gpioButtonReader{chip: filepath.Base(chipPath)}
		usable := true
		for pin := range buttons {
			if _, err := reader.read(pin); err != nil {
				usable = false
				break
			}
		}
		if usable {
			return reader, nil
		}
	}

	return gpioButtonReader{}, fmt.Errorf("gpioget could not read GPIO pins 5 and 6 from available gpiochips")
}

func (r gpioButtonReader) read(pin int) (string, error) {
	output, err := exec.Command("gpioget", r.chip, strconv.Itoa(pin)).Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(output))
	if value != "0" && value != "1" {
		return "", fmt.Errorf("unexpected gpioget value %q", value)
	}
	return value, nil
}

func waitForNextUpdate(tmpDir string, options AppOptions, frames int, refreshRate int, events <-chan inputEvent) {
	deadline := time.Now().Add(time.Duration(refreshRate) * time.Second)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}

		select {
		case event := <-events:
			switch event {
			case inputNext:
				return
			case inputStatus:
				statusPath, err := createStatusImage(tmpDir)
				if err != nil {
					fmt.Printf("Error creating status image: %v\n", err)
					continue
				}
				if err := displayImage(statusPath, options, frames); err != nil {
					fmt.Printf("Error displaying status image: %v\n", err)
				}
			}
		case <-time.After(remaining):
			return
		}
	}
}

func createStatusImage(tmpDir string) (string, error) {
	img := image.NewRGBA(image.Rect(0, 0, statusWidth, statusHeight))
	fillRect(img, 0, 0, statusWidth, statusHeight, color.RGBA{255, 255, 255, 255})
	fillRect(img, 0, 0, statusWidth, 78, color.RGBA{0, 0, 0, 255})

	now := time.Now().Format("MON JAN 2 2006 15:04:05 MST")
	wifi := getWifiSSID()
	machine := getMachineName()

	drawText(img, 34, 24, "STATUS", 6, color.RGBA{255, 255, 255, 255})
	drawText(img, 50, 140, "TIME", 4, color.RGBA{0, 0, 0, 255})
	drawText(img, 250, 140, now, 4, color.RGBA{0, 0, 0, 255})
	drawText(img, 50, 230, "WI-FI", 4, color.RGBA{0, 0, 0, 255})
	drawText(img, 250, 230, wifi, 4, color.RGBA{0, 0, 0, 255})
	drawText(img, 50, 320, "MACHINE", 4, color.RGBA{0, 0, 0, 255})
	drawText(img, 250, 320, machine, 4, color.RGBA{0, 0, 0, 255})

	statusPath := filepath.Join(tmpDir, "status.png")
	out, err := os.Create(statusPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if err := png.Encode(out, img); err != nil {
		return "", err
	}
	return statusPath, nil
}

func getWifiSSID() string {
	if output, err := exec.Command("iwgetid", "-r").Output(); err == nil {
		ssid := strings.TrimSpace(string(output))
		if ssid != "" {
			return ssid
		}
	}

	if output, err := exec.Command("nmcli", "-t", "-f", "active,ssid", "dev", "wifi").Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "yes:") {
				ssid := strings.TrimSpace(strings.TrimPrefix(line, "yes:"))
				if ssid != "" {
					return ssid
				}
			}
		}
	}

	return "unknown"
}

func getMachineName() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "unknown"
	}
	return hostname
}

func fillRect(img *image.RGBA, x int, y int, width int, height int, c color.RGBA) {
	for py := y; py < y+height; py++ {
		for px := x; px < x+width; px++ {
			if image.Pt(px, py).In(img.Bounds()) {
				img.SetRGBA(px, py, c)
			}
		}
	}
}

func drawText(img *image.RGBA, x int, y int, text string, scale int, c color.RGBA) {
	cursorX := x
	for _, r := range strings.ToUpper(text) {
		glyph, ok := font5x7[r]
		if !ok {
			glyph = font5x7['?']
		}
		for row, bits := range glyph {
			for col, bit := range bits {
				if bit == '1' {
					fillRect(img, cursorX+col*scale, y+row*scale, scale, scale, c)
				}
			}
		}
		cursorX += 6 * scale
	}
}

var font5x7 = map[rune][7]string{
	' ':  {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
	'!':  {"00100", "00100", "00100", "00100", "00100", "00000", "00100"},
	'"':  {"01010", "01010", "01010", "00000", "00000", "00000", "00000"},
	'#':  {"01010", "01010", "11111", "01010", "11111", "01010", "01010"},
	'$':  {"00100", "01111", "10100", "01110", "00101", "11110", "00100"},
	'%':  {"11001", "11010", "00100", "01000", "10110", "00110", "00000"},
	'&':  {"01100", "10010", "10100", "01000", "10101", "10010", "01101"},
	'\'': {"00100", "00100", "01000", "00000", "00000", "00000", "00000"},
	'(':  {"00010", "00100", "01000", "01000", "01000", "00100", "00010"},
	')':  {"01000", "00100", "00010", "00010", "00010", "00100", "01000"},
	'*':  {"00000", "00100", "10101", "01110", "10101", "00100", "00000"},
	'+':  {"00000", "00100", "00100", "11111", "00100", "00100", "00000"},
	',':  {"00000", "00000", "00000", "00000", "00100", "00100", "01000"},
	'-':  {"00000", "00000", "00000", "11111", "00000", "00000", "00000"},
	'.':  {"00000", "00000", "00000", "00000", "00000", "01100", "01100"},
	'/':  {"00001", "00010", "00100", "01000", "10000", "00000", "00000"},
	'0':  {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1':  {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2':  {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3':  {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4':  {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5':  {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6':  {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7':  {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8':  {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9':  {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	':':  {"00000", "01100", "01100", "00000", "01100", "01100", "00000"},
	';':  {"00000", "01100", "01100", "00000", "00100", "00100", "01000"},
	'<':  {"00010", "00100", "01000", "10000", "01000", "00100", "00010"},
	'=':  {"00000", "00000", "11111", "00000", "11111", "00000", "00000"},
	'>':  {"01000", "00100", "00010", "00001", "00010", "00100", "01000"},
	'?':  {"01110", "10001", "00001", "00010", "00100", "00000", "00100"},
	'@':  {"01110", "10001", "00001", "01101", "10101", "10101", "01110"},
	'A':  {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B':  {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C':  {"01110", "10001", "10000", "10000", "10000", "10001", "01110"},
	'D':  {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E':  {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F':  {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'G':  {"01110", "10001", "10000", "10111", "10001", "10001", "01110"},
	'H':  {"10001", "10001", "10001", "11111", "10001", "10001", "10001"},
	'I':  {"01110", "00100", "00100", "00100", "00100", "00100", "01110"},
	'J':  {"00001", "00001", "00001", "00001", "10001", "10001", "01110"},
	'K':  {"10001", "10010", "10100", "11000", "10100", "10010", "10001"},
	'L':  {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'M':  {"10001", "11011", "10101", "10101", "10001", "10001", "10001"},
	'N':  {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O':  {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P':  {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'Q':  {"01110", "10001", "10001", "10001", "10101", "10010", "01101"},
	'R':  {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'S':  {"01111", "10000", "10000", "01110", "00001", "00001", "11110"},
	'T':  {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'U':  {"10001", "10001", "10001", "10001", "10001", "10001", "01110"},
	'V':  {"10001", "10001", "10001", "10001", "10001", "01010", "00100"},
	'W':  {"10001", "10001", "10001", "10101", "10101", "10101", "01010"},
	'X':  {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
	'Y':  {"10001", "10001", "01010", "00100", "00100", "00100", "00100"},
	'Z':  {"11111", "00001", "00010", "00100", "01000", "10000", "11111"},
	'[':  {"01110", "01000", "01000", "01000", "01000", "01000", "01110"},
	'\\': {"10000", "01000", "00100", "00010", "00001", "00000", "00000"},
	']':  {"01110", "00010", "00010", "00010", "00010", "00010", "01110"},
	'^':  {"00100", "01010", "10001", "00000", "00000", "00000", "00000"},
	'_':  {"00000", "00000", "00000", "00000", "00000", "00000", "11111"},
}

func displayImage(imagePath string, options AppOptions, frames int) error {
	//
	// N.B (Larry Bank)
	// This update can use one of 3 temperature/panel profiles
	// and the 3 update modes for 1-bit content
	// Please consider if this should have a counter and mimic the TRMNL-OG behavior
	//
	var sb strings.Builder
	var sb2 strings.Builder
	var sb3 strings.Builder

	sb.WriteString("file=")
	sb.WriteString(imagePath)

	sb2.WriteString("invert=")
	if options.DarkMode {
		sb2.WriteString("true")
	} else {
		sb2.WriteString("false")
	}

	sb3.WriteString("mode=")
	if (frames & 3) == 0 { // use fast mode every 4 updates to clear any ghosting
		sb3.WriteString("fast")
	} else {
		sb3.WriteString("partial") // partial = no flicker/flash
	}
	err := exec.Command("show_img", sb.String(), sb2.String(), sb3.String()).Run()
	if err != nil {
		fmt.Printf("show_img tool missing; build it and try again; error = %v\n", err)
		os.Exit(0)
	}
	if options.Verbose {
		fmt.Printf("Displayed: %s\n", imagePath)
		fmt.Println("EPD update completed")
	}
	return nil
}

func loadConfig(configDir string) Config {
	configFile := filepath.Join(configDir, "config.json")
	config := Config{}

	data, err := os.ReadFile(configFile)
	if err != nil {
		return config
	}

	_ = json.Unmarshal(data, &config)
	return config
}

func saveConfig(configDir string, config Config) {
	configFile := filepath.Join(configDir, "config.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		return
	}

	err = os.WriteFile(configFile, data, 0600)
	if err != nil {
		fmt.Printf("Error writing config file: %v\n", err)
	}
}
