package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	eng "github.com/named-data/ndnd/std/engine"
	"github.com/named-data/ndnd/std/log"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/object"
	"github.com/named-data/ndnd/std/object/storage"
	"github.com/named-data/ndnd/std/sync"
)

func main() {
	log.Default().SetLevel(log.LevelTrace)
	prefixStr := flag.String("prefix", "/minindn/svs", "SVS group prefix")
	nameStr := flag.String("name", "", "Participant name")
	msgStr := flag.String("msg", "", "Message to send")
	delaySecs := flag.Int("delay", 0, "Wait N seconds before sending the message")
	waitSecs := flag.Int("wait", 10, "Seconds to wait before exiting")
	flag.Parse()

	if *nameStr == "" {
		fmt.Println("Error: -name is required")
		os.Exit(1)
	}

	groupPrefix, _ := enc.NameFromStr(*prefixStr)
	nodeName, _ := enc.NameFromStr(*nameStr)

	app := eng.NewBasicEngine(eng.NewDefaultFace())
	if err := app.Start(); err != nil {
		fmt.Printf("Failed to start engine: %v\n", err)
		os.Exit(1)
	}
	defer app.Stop()

	store := storage.NewMemoryStore()
	client := object.NewClient(app, store, nil)

	err := client.Start()
	if err != nil {
		panic(err)
	}
	defer client.Stop()

	syncPrefix := groupPrefix.Append(enc.NewKeywordComponent("svs"))
	client.AnnouncePrefix(ndn.Announcement{Name: syncPrefix, Expose: true, Multicast: true})

	dataPrefix := groupPrefix.Append(nodeName...)
	client.AnnouncePrefix(ndn.Announcement{Name: dataPrefix, Expose: true})

	alo, err := sync.NewSvsALO(sync.SvsAloOpts{
		Name: nodeName,
		Svs: sync.SvSyncOpts{
			Client:      client,
			GroupPrefix: groupPrefix,
			BootTime:    1,
		},
		Snapshot: &sync.SnapshotNodeLatest{
			Client: client,
			SnapMe: func(_ enc.Name) (enc.Wire, error) {
				if *msgStr != "" {
					return enc.Wire{[]byte(*msgStr)}, nil
				}
				return enc.Wire{[]byte("(no message)")}, nil
			},
			Threshold: 5,
		},
	})
	if err != nil {
		panic(err)
	}

	alo.SetOnPublisher(func(name enc.Name) {
		alo.SubscribePublisher(name, func(pub sync.SvsPub) {
			fmt.Printf("CHAT %s: %s\n", pub.Publisher.String(), string(pub.Bytes()))
		})
	})

	if err := alo.Start(); err != nil {
		panic(err)
	}

	// Important: let the sync initialization happen
	time.Sleep(2 * time.Second)

	if *msgStr != "" {
		if *delaySecs > 0 {
			time.Sleep(time.Duration(*delaySecs) * time.Second)
		}
		_, _, err := alo.Publish(enc.Wire{[]byte(*msgStr)})
		if err != nil {
			fmt.Printf("Publish error: %v\n", err)
		} else {
			fmt.Printf("Published message: %s\n", *msgStr)
		}
	}

	time.Sleep(time.Duration(*waitSecs) * time.Second)
	alo.Stop()
}
