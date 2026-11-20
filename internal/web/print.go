package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ibm3486/internal/printer"
)

// Print jobs: the emulated terminal's printer output (emu.SetPrintHandler)
// is kept per browser session; the page asks for a job as PDF or text to
// save it, or has it printed on one of this computer's printers (CUPS,
// macOS and Linux) with lp.

// maxJobs is how many finished jobs a session keeps for the page.
const maxJobs = 8

type printJob struct {
	id   int
	doc  *printer.Document
	name string // suggested file name, without extension
}

// jobStore holds a session's recent print jobs. The machine goroutine
// adds jobs; the WebSocket loop reads them.
type jobStore struct {
	mu   sync.Mutex
	next int
	jobs []*printJob
}

func (s *jobStore) add(doc *printer.Document) *printJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	j := &printJob{id: s.next, doc: doc,
		name: "ibm3477-print-" + time.Now().Format("2006-01-02-150405")}
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > maxJobs {
		s.jobs = s.jobs[1:]
	}
	return j
}

func (s *jobStore) get(id int) *printJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

func (s *jobStore) drop(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.id == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			return
		}
	}
}

// jobFile returns a job as a file: "pdf" or "txt".
func jobFile(j *printJob, format string) (data []byte, name string) {
	if format == "txt" {
		return []byte(j.doc.Text()), j.name + ".txt"
	}
	return j.doc.PDF(), j.name + ".pdf"
}

// lookPath and runCmd are the external commands used (replaced in tests).
var (
	lookPath = exec.LookPath
	runCmd   = func(name string, stdin []byte, args ...string) ([]byte, error) {
		// never hang a session on a stuck print system
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		return cmd.CombinedOutput()
	}
)

// listPrinters returns this computer's printers and the default one from
// CUPS (lpstat), or none where CUPS is not installed.
func listPrinters() (names []string, def string) {
	if _, err := lookPath("lpstat"); err != nil {
		return nil, ""
	}
	if out, err := runCmd("lpstat", nil, "-e"); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if n := strings.TrimSpace(l); n != "" {
				names = append(names, n)
			}
		}
	}
	// "system default destination: NAME"
	if out, err := runCmd("lpstat", nil, "-d"); err == nil {
		if i := strings.LastIndex(string(out), ": "); i >= 0 {
			def = strings.TrimSpace(string(out)[i+2:])
		}
	}
	return names, def
}

// printJobOn prints a job's PDF on a CUPS printer. The name must be one
// lpstat lists: it is never passed through unchecked.
func printJobOn(j *printJob, printerName string) error {
	names, _ := listPrinters()
	known := false
	for _, n := range names {
		known = known || n == printerName
	}
	if !known {
		return fmt.Errorf("no printer %q", printerName)
	}
	out, err := runCmd("lp", j.doc.PDF(), "-d", printerName, "-t", j.name)
	if err != nil {
		return fmt.Errorf("lp: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// encodeFile is the base64 form of a job file for the page.
func encodeFile(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
