package budget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"
)

const (
	recordVersion = 1
	recordFile    = "spend.json"
	backupFile    = "spend.json.bak"
)

// record is the spend total as saved to disk.
type record struct {
	Version   int       `json:"version"`
	Period    string    `json:"period"`
	SpentUSD  float64   `json:"spent_usd"`
	Calls     int64     `json:"calls"`
	UpdatedAt time.Time `json:"updated_at"`
}

// loadStatus says where a loaded record came from.
type loadStatus int

const (
	loadMissing    loadStatus = iota // no record yet, a normal first run
	loadOK                           // main file read fine
	loadFromBackup                   // main file was damaged or missing, backup used
	loadUnreadable                   // nothing readable, needs recovery
)

// ledger saves the spend record to a directory and reads it back. Every
// save writes a temp file and renames it into place, so a crash can never
// leave a half-written record. The previous good record is kept as a
// backup.
type ledger struct {
	dir    string
	path   string
	backup string

	// last holds the bytes of the most recent good record, written to the
	// backup on the next save.
	last []byte
}

// newLedger prepares dir for saving and checks it is writable.
func newLedger(dir string) (*ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create data directory %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return nil, fmt.Errorf("data directory %s is not writable: %w", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())

	return &ledger{
		dir:    dir,
		path:   filepath.Join(dir, recordFile),
		backup: filepath.Join(dir, backupFile),
	}, nil
}

// load reads the saved record, falling back to the backup if the main file
// is damaged or missing.
func (l *ledger) load() (record, loadStatus) {
	mainRec, mainRaw, mainErr := readRecord(l.path)
	if mainErr == nil {
		l.last = mainRaw
		return mainRec, loadOK
	}

	backRec, backRaw, backErr := readRecord(l.backup)
	if backErr == nil {
		l.last = backRaw
		return backRec, loadFromBackup
	}

	if errors.Is(mainErr, fs.ErrNotExist) && errors.Is(backErr, fs.ErrNotExist) {
		return record{}, loadMissing
	}
	return record{}, loadUnreadable
}

// save writes r as the current record. The backup is updated first, so a
// crash between the two writes leaves both files readable.
func (l *ledger) save(r record) error {
	r.Version = recordVersion
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}

	if l.last != nil {
		if err := writeAtomic(l.backup, l.last); err != nil {
			return fmt.Errorf("saving spend backup: %w", err)
		}
	}
	if err := writeAtomic(l.path, raw); err != nil {
		return fmt.Errorf("saving spend record: %w", err)
	}
	l.last = raw
	return nil
}

// readRecord reads and validates one record file. The raw bytes are
// returned so they can become the next backup unchanged.
func readRecord(path string) (record, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return record{}, nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return record{}, nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if err := r.validate(); err != nil {
		return record{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, raw, nil
}

func (r record) validate() error {
	switch {
	case r.Version != recordVersion:
		return fmt.Errorf("unknown record version %d", r.Version)
	case r.Period == "":
		return errors.New("record has no period")
	case r.SpentUSD < 0 || math.IsNaN(r.SpentUSD) || math.IsInf(r.SpentUSD, 0):
		return fmt.Errorf("record has an invalid spend of %v", r.SpentUSD)
	case r.Calls < 0:
		return fmt.Errorf("record has an invalid call count of %d", r.Calls)
	}
	return nil
}

// writeAtomic writes data to a temp file in the same directory, flushes it
// to disk, then renames it over path.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".spend-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Flush the directory entry too, so the rename itself survives a power
	// cut. Not supported on every platform, so failure is ignored.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
