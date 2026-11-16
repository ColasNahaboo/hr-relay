// hr-relay — BLE heart-rate relay.
//
// Central role: connects to a standard BLE heart-rate sensor (a COROS strap
// here) and subscribes to its Heart Rate Measurement characteristic.
// Peripheral role: advertises the standard Heart Rate service and forwards
// every measurement to any connected GATT client, e.g. Chrome's Web
// Bluetooth on the phone. The phone sees an ordinary heart-rate belt, so
// walkpadspeed needs no changes.
//
// Why it exists: Chrome's Web Bluetooth path to the strap can wedge — the
// link attaches instantly to a stale connection and startNotifications
// never completes — while other BLE stacks (this machine's BlueZ, nRF
// Connect on the phone) talk to the same strap fine. The relay moves the
// strap subscription to BlueZ here, and the phone only ever talks to this
// machine.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"tinygo.org/x/bluetooth"
)

var adapter = bluetooth.DefaultAdapter

var (
	strapAddr = flag.String("addr", "D1:F9:A6:A2:FC:6D",
		"sensor address; empty to match by name prefix instead")
	strapName = flag.String("name", "COROS HEART RATE",
		"sensor advertised-name prefix (used when -addr is empty)")
	advName = flag.String("advertise-name", "HR RELAY",
		"name shown in the phone's device chooser")
	retrySecs = flag.Int("retry-secs", 5, "seconds between sensor reconnect attempts")
	staleSecs = flag.Int("stale-secs", 60,
		"drop the sensor link after this long without data (0 disables)")
	debug = flag.Bool("debug", false, "log every heart-rate notification")
)

func must(what string, err error) {
	if err != nil {
		log.Fatalf("fatal: %s: %v", what, err)
	}
}

// parseBPM extracts the heart rate from a Heart Rate Measurement payload:
// a flags byte, then a uint8 (or uint16, flags bit 0) bpm value.
func parseBPM(data []byte) int {
	if len(data) < 2 {
		return 0
	}
	if data[0]&0x01 != 0 {
		if len(data) < 3 {
			return 0
		}
		return int(data[1]) | int(data[2])<<8
	}
	return int(data[1])
}

type relay struct {
	mu       sync.Mutex
	hrm      *bluetooth.Characteristic // server-side characteristic to notify
	lastBeat time.Time
	lastBPM  int
}

// onSensorData forwards one measurement to all connected clients (the
// phone's Chrome) and keeps the staleness watchdog fed.
func (r *relay) onSensorData(data []byte) {
	r.mu.Lock()
	r.lastBeat = time.Now()
	bpm := parseBPM(data)
	changed := bpm != r.lastBPM
	r.lastBPM = bpm
	r.mu.Unlock()

	if changed {
		log.Printf("HR %d bpm", bpm)
	}
	if *debug {
		log.Printf("HR raw % x", data)
	}
	if r.hrm != nil {
		if _, err := r.hrm.Write(data); err != nil {
			log.Printf("forwarding to clients failed: %v", err)
		}
	}
}

// startPeripheral registers the standard Heart Rate service (0x180D); its
// measurement characteristic is what the phone's Chrome subscribes to.
func (r *relay) startPeripheral() {
	var hrm bluetooth.Characteristic
	must("register heart-rate service", adapter.AddService(&bluetooth.Service{
		UUID: bluetooth.ServiceUUIDHeartRate,
		Characteristics: []bluetooth.CharacteristicConfig{{
			Handle: &hrm,
			UUID:   bluetooth.CharacteristicUUIDHeartRateMeasurement,
			Value:  []byte{0, 0},
			Flags:  bluetooth.CharacteristicNotifyPermission | bluetooth.CharacteristicReadPermission,
		}},
	}))
	r.mu.Lock()
	r.hrm = &hrm
	r.mu.Unlock()
}

// startAdvertising makes the relay visible in the phone's device chooser.
// The Heart Rate service UUID in the payload is what Chrome's
// requestDevice({services: ['heart_rate']}) filter matches.
func (r *relay) startAdvertising() {
	adv := adapter.DefaultAdvertisement()
	must("configure advertisement", adv.Configure(bluetooth.AdvertisementOptions{
		LocalName:    *advName,
		ServiceUUIDs: []bluetooth.UUID{bluetooth.ServiceUUIDHeartRate},
	}))
	must("start advertisement", adv.Start())
	log.Printf("advertising as %q (heart-rate service 0x180D)", *advName)
}

// connectSensor scans until the strap is seen advertising, then connects and
// resolves the Heart Rate Measurement characteristic.
func (r *relay) connectSensor() (bluetooth.Device, bluetooth.DeviceCharacteristic, error) {
	fail := func(err error) (bluetooth.Device, bluetooth.DeviceCharacteristic, error) {
		return bluetooth.Device{}, bluetooth.DeviceCharacteristic{}, err
	}

	results := make(chan bluetooth.ScanResult, 1)
	var mu sync.Mutex
	found := false

	log.Printf("scanning for sensor (%s)...", sensorLabel())
	if err := adapter.Scan(func(a *bluetooth.Adapter, res bluetooth.ScanResult) {
		mu.Lock()
		defer mu.Unlock()
		if found || !matchSensor(res) {
			return
		}
		found = true
		a.StopScan()
		select {
		case results <- res:
		default:
		}
	}); err != nil {
		return fail(fmt.Errorf("scan: %w", err))
	}

	var res bluetooth.ScanResult
	select {
	case res = <-results:
	case <-time.After(15 * time.Second):
		adapter.StopScan()
		return fail(fmt.Errorf("not advertising"))
	}
	log.Printf("sensor found: %s (%s, %d dBm)", res.LocalName(), res.Address.String(), res.RSSI)

	dev, err := adapter.Connect(res.Address, bluetooth.ConnectionParams{})
	if err != nil {
		return fail(fmt.Errorf("connect: %w", err))
	}

	srvcs, err := dev.DiscoverServices([]bluetooth.UUID{bluetooth.ServiceUUIDHeartRate})
	if err != nil {
		dev.Disconnect()
		return fail(fmt.Errorf("discover services: %w", err))
	}
	if len(srvcs) == 0 {
		dev.Disconnect()
		return fail(fmt.Errorf("no heart-rate service"))
	}
	chars, err := srvcs[0].DiscoverCharacteristics([]bluetooth.UUID{bluetooth.CharacteristicUUIDHeartRateMeasurement})
	if err != nil {
		dev.Disconnect()
		return fail(fmt.Errorf("discover characteristics: %w", err))
	}
	if len(chars) == 0 {
		dev.Disconnect()
		return fail(fmt.Errorf("no heart-rate measurement characteristic"))
	}
	return dev, chars[0], nil
}

// forward subscribes to the sensor's notifications and blocks until the link
// goes bad (no data for staleSecs) or the subscription errors.
func (r *relay) forward(dev bluetooth.Device, char bluetooth.DeviceCharacteristic) error {
	r.mu.Lock()
	r.lastBeat = time.Now()
	r.lastBPM = 0
	r.mu.Unlock()

	if err := char.EnableNotifications(func(buf []byte) {
		r.onSensorData(buf)
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("subscribed to sensor heart-rate notifications")

	for {
		time.Sleep(time.Second)
		if *staleSecs <= 0 {
			continue
		}
		r.mu.Lock()
		last := r.lastBeat
		r.mu.Unlock()
		if time.Since(last) > time.Duration(*staleSecs)*time.Second {
			return fmt.Errorf("no heart-rate data for %s (strap off/asleep or link dead)",
				time.Since(last).Round(time.Second))
		}
	}
}

func matchSensor(res bluetooth.ScanResult) bool {
	if *strapAddr != "" {
		return res.Address.String() == strings.ToUpper(*strapAddr)
	}
	return strings.HasPrefix(res.LocalName(), *strapName)
}

func sensorLabel() string {
	if *strapAddr != "" {
		return *strapAddr
	}
	return "name " + *strapName
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime)

	must("enable BLE stack", adapter.Enable())

	r := &relay{lastBeat: time.Now()}

	// Must be set before startAdvertising: the BlueZ backend only starts its
	// D-Bus signal loop when a handler is already registered, so without this
	// the relay is blind to the phone connecting or dropping.
	//
	// BlueZ also fires Connected=false property changes for devices merely
	// discovered while scanning, so only a drop of a client we saw connect is
	// a real disconnect.
	var clientMu sync.Mutex
	clients := map[string]bool{}
	adapter.SetConnectionStateHandler(func(dev bluetooth.Device, state bluetooth.ConnectionState) {
		addr := dev.Address.String()
		clientMu.Lock()
		defer clientMu.Unlock()
		if state.Connected {
			clients[addr] = true
			log.Printf("client connected: %s", addr)
			return
		}
		if !clients[addr] {
			return
		}
		delete(clients, addr)
		why := "disconnected"
		if state.Err != nil {
			why = state.Err.Error()
		}
		log.Printf("client disconnected: %s (%s)", addr, why)
	})

	r.startAdvertising()
	r.startPeripheral()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		os.Exit(0)
	}()

	for {
		dev, char, err := r.connectSensor()
		if err != nil {
			log.Printf("sensor not available: %v — retrying in %ds", err, *retrySecs)
			time.Sleep(time.Duration(*retrySecs) * time.Second)
			continue
		}
		log.Printf("sensor connected: %s", dev.Address.String())
		if err := r.forward(dev, char); err != nil {
			log.Printf("sensor link lost: %v — reconnecting", err)
		}
		dev.Disconnect()
		time.Sleep(time.Duration(*retrySecs) * time.Second)
	}
}
