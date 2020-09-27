package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bemasher/sx1276"
	"golang.org/x/xerrors"

	influxdb2 "github.com/influxdata/influxdb-client-go"
)

const (
	frf = 904.5e6

	bw = 125

	vref = 1.5
	r1   = 300e3
	r2   = 180e3
	vdiv = r2 / (r1 + r2)

	dryrun = false

	wdtPeriod = 2 * time.Minute
)

type ID byte

func (id ID) MarshalText() ([]byte, error) {
	hexBuf := make([]byte, hex.EncodedLen(1))
	hex.Encode(hexBuf, []byte{byte(id)})
	return hexBuf, nil
}

func (id *ID) UnmarshalText(text []byte) error {
	if hex.DecodedLen(len(text)) != 1 {
		return xerrors.Errorf("invalid id length: %q\n", text)
	}

	hexBuf := make([]byte, hex.DecodedLen(len(text)))
	_, err := hex.Decode(hexBuf, text)
	if err != nil {
		return xerrors.Errorf("hex.DecodeString: %w", err)
	}

	*id = ID(hexBuf[0])

	return nil
}

type Device struct {
	Name   string
	BME280 BME280
}

type Config map[ID]Device

func (cfg *Config) Read(filename string) error {
	cfgBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return xerrors.Errorf("ioutil.ReadFile: %w", err)
	}
	err = json.Unmarshal(cfgBytes, cfg)
	if err != nil {
		return xerrors.Errorf("json.Decode: %w", err)
	}

	return nil
}

func (cfg Config) Write(filename string) error {
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return xerrors.Errorf("json.MarshalIndent: %w", err)
	}

	err = ioutil.WriteFile(filename, cfgBytes, 0600)
	if err != nil {
		return xerrors.Errorf("ioutil.WriteFile: %w", err)
	}

	return nil
}

func (cfg *Config) Reload(filename string) error {
	// Read config from disk.
	newCfg := Config{}
	err := newCfg.Read(filename)
	if err != nil {
		return xerrors.Errorf("newCfg.Read: %w", err)
	}

	// Merge new config with current.
	for id, dev := range newCfg {
		(*cfg)[id] = dev
	}

	// Commit merged config to disk.
	err = cfg.Write(filename)
	if err != nil {
		return xerrors.Errorf("cfg.Write: %w", err)
	}

	return nil
}

var (
	deviceFilename string

	hostname  string
	username  string
	password  string
	database  string
	measure   string
	retention string
)

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)

	var ok bool

	if deviceFilename, ok = os.LookupEnv("DATMOS_DEVICES"); !ok {
		log.Fatalln("required environment variable DATMOS_DEVICES undefined")
	}
	log.Printf("DATMOS_DEVICES=%q\n", deviceFilename)

	if hostname, ok = os.LookupEnv("DATMOS_HOSTNAME"); !ok {
		log.Fatalln("required environment variable DATMOS_HOSTNAME undefined")
	}
	log.Printf("DATMOS_HOSTNAME=%q\n", hostname)

	username = os.Getenv("DATMOS_USERNAME")
	log.Printf("DATMOS_USERNAME=%q\n", username)

	password = os.Getenv("DATMOS_PASSWORD")
	log.Println("DATMOS_PASSWORD=************")

	if database, ok = os.LookupEnv("DATMOS_DATABASE"); !ok {
		database = "datmos"
	}
	log.Printf("DATMOS_DATABASE=%q\n", database)

	retention, _ = os.LookupEnv("DATMOS_RETENTION")
	log.Printf("DATMOS_RETENTION=%q\n", retention)

	if measure, ok = os.LookupEnv("DATMOS_MEASURE"); !ok {
		retention = "environment"
	}
	log.Printf("DATMOS_MEASURE=%q\n", measure)
}

func main() {
	cfg := Config{}
	err := cfg.Read(deviceFilename)
	if os.IsNotExist(err) {
		log.Printf("device file does not exist, will write one on exit")
	}
	if err != nil {
		log.Fatalf("%+v\n", xerrors.Errorf("cfg.Read: %w", err))
	}
	defer func() {
		// Save config on exit.
		err := cfg.Write(deviceFilename)
		if err != nil {
			log.Fatalf("%+v\n", xerrors.Errorf("cfg.Write: %w", err))
		}
	}()

	for id, dev := range cfg {
		log.Printf("{ID:%02X Name:%q}\n", id, dev.Name)
	}

	token := ""
	if username != "" {
		token = username + ":" + password
	}

	client := influxdb2.NewClient(hostname, token)
	if err != nil {
		log.Fatalf("%+v\n", xerrors.Errorf("influxdb2.NewClient: %w", err))
	}
	defer client.Close()

	bucket := database
	if retention != "" {
		bucket += "/" + retention
	}
	influxWriter := client.WriteAPIBlocking("", bucket)

	sig := make(chan os.Signal)
	signal.Notify(sig, os.Interrupt, os.Kill)

	sx, err := sx1276.NewSX1276()
	if err != nil {
		log.Fatalf("%+v\n", xerrors.Errorf("NewSX1276: %w", err))
	}
	defer sx.Close()

	sx.WriteReg(sx1276.RegLoRaOPMODE, 0|
		sx1276.LORA_OPMODE_LONGRANGEMODE_ON|
		sx1276.LORA_OPMODE_SLEEP,
	)

	sx.WriteReg(sx1276.RegLoRaMODEMCONFIG2, 0|
		sx1276.LORA_MODEMCONFIG2_SF_7,
	)

	sx.WriteReg(sx1276.LORA_PAYLOADMAXLENGTH, 0x80)

	sx.SetFreq(frf)

	rxCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pkts := sx.StartRX(rxCtx)
	defer sx.SetMode(sx1276.LORA_OPMODE_SLEEP)

	// Watch dog timer.
	wdt := time.NewTimer(wdtPeriod)

	sigCh := make(chan os.Signal)
	signal.Notify(sigCh)

	log.Println("listening...")

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGUSR1:
				log.Println("reloading config...")
				err = cfg.Reload(deviceFilename)
				if err != nil {
					log.Printf("%+v\n", xerrors.Errorf("cfg.Write: %w", err))
				}
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
				log.Printf("received signal: %+v\n", sig)
				return
			}
		case <-rxCtx.Done():
			log.Println("context cancelled...")
			return
		case <-wdt.C:
			log.Println("wdt elapsed...")
			cancel()
			sx.SetMode(sx1276.LORA_OPMODE_SLEEP)

			rxCtx, cancel = context.WithCancel(context.Background())
			defer cancel()

			pkts = sx.StartRX(rxCtx)
			defer sx.SetMode(sx1276.LORA_OPMODE_SLEEP)

			wdt.Reset(wdtPeriod)
		case pkt := <-pkts:
			if !wdt.Stop() {
				<-wdt.C
			}
			wdt.Reset(wdtPeriod)

			var (
				id  ID
				dev Device
			)

			t := time.Now()

			switch len(pkt) {
			case 44:
				id = ID(pkt[0])
				log.Printf("ID:0x%02X calibrating...", id)

				dev = cfg[id]
				dev.BME280.Cal(pkt[1:])
				cfg[id] = dev

				pkt = pkt[34:]
			case 11:
				id = ID(pkt[0])
				pkt = pkt[1:]

				var ok bool

				if dev, ok = cfg[id]; !ok {
					log.Printf("ID:0x%02X not calibrated\n", id)
					continue
				}
			default:
				log.Printf("Unhandled Length (%2d): %02X\n", len(pkt), pkt)
				continue
			}

			dev.BME280.Update(pkt)

			rssi := sx.PktRSSI()
			snr := sx.PktSNR()
			fei := sx.PktFEI(bw)

			adc := binary.LittleEndian.Uint16(pkt[8:10])
			vbat := float64(adc) * vref / 1023 / vdiv / 64

			name := "unnamed"
			if dev.Name != "" {
				name = dev.Name
			}

			log.Printf(
				"ID:0x%02X Name:%q T:%0.1fF H:%0.1f%% P:%0.1fhPa V:%0.3fV\n",
				id, name,
				dev.BME280.temperature,
				dev.BME280.humidity,
				dev.BME280.pressure,
				vbat,
			)

			pt := influxdb2.NewPoint(measure,
				map[string]string{
					"id":   fmt.Sprintf("%02X", id),
					"name": name,
				},
				map[string]interface{}{
					"temperature": dev.BME280.temperature,
					"humidity":    dev.BME280.humidity,
					"pressure":    dev.BME280.pressure,
					"rssi":        rssi,
					"snr":         snr,
					"fei":         fei,
					"vbat":        vbat,
				},
				t,
			)
			if err != nil {
				log.Printf("%+v\n", xerrors.Errorf("influxdb2.NewPoint: %w", err))
				continue
			}

			if dryrun {
				continue
			}

			err := influxWriter.WritePoint(context.Background(), pt)
			if err != nil {
				log.Printf("%+v\n", xerrors.Errorf("influxWriter.WritePoint: %w", err))
				continue
			}
		}
	}
}
