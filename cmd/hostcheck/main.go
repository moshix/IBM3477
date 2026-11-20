// Command hostcheck connects the TN5250 host side alone (no terminal) to a
// host and prints the first screen it receives, to check connectivity and
// the 5250 data stream handling. Lines are cut to 79 columns.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"ibm3486/internal/tn5250"
)

func main() {
	host := flag.String("host", "pub400.com", "TN5250 host")
	port := flag.String("port", "23", "TN5250 port")
	wait := flag.Duration("wait", 6*time.Second, "how long to wait for the screen")
	flag.Parse()
	c := tn5250.NewClient("3179-2", "37")
	var mu sync.Mutex
	var snap *tn5250.Snapshot
	c.SetUpdateCallback(func(s *tn5250.Snapshot) {
		mu.Lock()
		snap = s
		mu.Unlock()
	})
	if err := c.Connect(*host, *port, false); err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	go c.Run()
	time.Sleep(*wait)
	s := c.Screen()
	mu.Lock()
	defer mu.Unlock()
	if snap == nil {
		fmt.Fprintln(os.Stderr, "no screen received")
		os.Exit(1)
	}
	for r := 0; r < snap.Rows; r++ {
		var b strings.Builder
		for col := 0; col < snap.Cols && col < 79; col++ {
			ch := snap.Cells[r*snap.Cols+col].Char
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
	fmt.Printf("cursor %d,%d inhibited=%v\n", s.CursorRow, s.CursorCol, s.Inhibited)
	// 5250 attribute bytes (0x20-0x3F) per row, as the host sent them
	for r := 0; r < s.Rows; r++ {
		var at []string
		for col := 0; col < s.Cols; col++ {
			if b := s.Data[r*s.Cols+col]; b >= 0x20 && b <= 0x3F {
				at = append(at, fmt.Sprintf("%d:%02X", col, b))
			}
		}
		if len(at) > 0 {
			fmt.Printf("row %2d attributes %v\n", r, at)
		}
	}
	c.Disconnect()
}
