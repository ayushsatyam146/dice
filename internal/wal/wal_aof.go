package wal

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	sync "sync"
	"time"

	"github.com/dicedb/dice/config"
	"github.com/dicedb/dice/internal/cmd"
)

const (
	segmentPrefix           = "seg-"
	defaultVersion          = "v0.0.1"
	versionTagSize          = 1 // Tag for "version" field
	versionLengthPrefixSize = 1 // Length prefix for "version"
	versionSize             = 6 // Fixed size for "v0.0.1"
	logSequenceNumberSize   = 8
	dataTagSize             = 1 // Tag for "data" field
	dataLengthPrefixSize    = 1 // Length prefix for "data"
	CRCSize                 = 4
	timestampSize           = 8
)

type WALAOF struct {
	logDir                 string
	currentSegmentFile     *os.File
	walMode                string
	writeMode              string
	maxSegmentSize         int64
	maxSegmentCount        int
	currentSegmentIndex    int
	oldestSegmentIndex     int
	byteOffset             int
	bufferSize             int
	retentionMode          string
	recoveryMode           string
	rotationMode           string
	lastSequenceNo         uint64
	bufWriter              *bufio.Writer
	bufferSyncTicker       *time.Ticker
	segmentRotationTicker  *time.Ticker
	segmentRetentionTicker *time.Ticker
	lock                   sync.Mutex
	ctx                    context.Context
	cancel                 context.CancelFunc
}

func NewAOFWAL(directory string) (*WALAOF, error) {
	ctx, cancel := context.WithCancel(context.Background())

	return &WALAOF{
		logDir:                 directory,
		walMode:                config.DiceConfig.WAL.WalMode,
		bufferSyncTicker:       time.NewTicker(config.DiceConfig.WAL.BufferSyncInterval * time.Millisecond),
		segmentRotationTicker:  time.NewTicker(config.DiceConfig.WAL.SegmentRotationTime * time.Second),
		segmentRetentionTicker: time.NewTicker(config.DiceConfig.WAL.SegmentRetentionDuration * time.Second),
		writeMode:              config.DiceConfig.WAL.WriteMode,
		maxSegmentSize:         config.DiceConfig.WAL.MaxSegmentSizeMB * 1024 * 1024,
		maxSegmentCount:        config.DiceConfig.WAL.MaxSegmentCount,
		bufferSize:             config.DiceConfig.WAL.BufferSizeMB * 1024 * 1024,
		retentionMode:          config.DiceConfig.WAL.RetentionMode,
		recoveryMode:           config.DiceConfig.WAL.RecoveryMode,
		rotationMode:           config.DiceConfig.WAL.RotationMode,
		ctx:                    ctx,
		cancel:                 cancel,
	}, nil
}

func (wal *WALAOF) Init(t time.Time) error {
	if err := wal.validateConfig(); err != nil {
		return err
	}

	// TODO - Restore existing checkpoints to memory

	// Create the directory if it doesn't exist
	if err := os.MkdirAll(wal.logDir, 0755); err != nil {
		return nil
	}

	// Get the list of log segment files in the directory
	files, err := filepath.Glob(filepath.Join(wal.logDir, segmentPrefix+"*"))
	if err != nil {
		return nil
	}

	if len(files) > 0 {
		fmt.Println("Found existing log segments:", files)
		// TODO - Check if we have newer WAL entries after the last checkpoint and simultaneously replay and checkpoint them
	}

	var wg sync.WaitGroup
	errCh := make(chan error, wal.maxSegmentCount)

	for i := 0; i < wal.maxSegmentCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			filePath := filepath.Join(wal.logDir, segmentPrefix+fmt.Sprintf("-%d", index))
			file, err := os.Create(filePath)
			if err != nil {
				errCh <- fmt.Errorf("error creating segment file %s: %v", filePath, err)
				return
			}
			defer file.Close()
		}(i)
	}

	wg.Wait()
	close(errCh)

	wal.lastSequenceNo = 0
	wal.currentSegmentIndex = 0
	wal.oldestSegmentIndex = 0
	wal.byteOffset = 0
	wal.currentSegmentFile, err = os.OpenFile(filepath.Join(wal.logDir, "seg-0"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if _, err := wal.currentSegmentFile.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	wal.bufWriter = bufio.NewWriterSize(wal.currentSegmentFile, wal.bufferSize)

	go wal.keepSyncingBuffer()

	if wal.rotationMode == "time" { //nolint:goconst
		go wal.rotateSegmentPeriodically()
	}

	if wal.retentionMode == "time" { //nolint:goconst
		go wal.deleteSegmentPeriodically()
	}

	return nil
}

// WriteEntry writes an entry to the WAL.
func (wal *WALAOF) LogCommand(data []byte) error {
	return wal.writeEntry(data)
}

func (wal *WALAOF) writeEntry(data []byte) error {
	wal.lock.Lock()
	defer wal.lock.Unlock()

	wal.lastSequenceNo++
	entry := &WAL_Entry{
		Version:           defaultVersion,
		LogSequenceNumber: wal.lastSequenceNo,
		Data:              data,
		CRC:               crc32.ChecksumIEEE(append(data, byte(wal.lastSequenceNo))),
		Timestamp:         time.Now().UnixNano(),
	}

	entrySize := getEntrySize(data)
	if err := wal.rotateLogIfNeeded(entrySize); err != nil {
		return err
	}

	wal.byteOffset += entrySize

	if err := wal.writeEntryToBuffer(entry); err != nil {
		return err
	}

	// if wal-mode unbuffered immediately sync to disk
	if wal.walMode == "unbuffered" { //nolint:goconst
		if err := wal.Sync(); err != nil {
			return err
		}
	}

	return nil
}

func (wal *WALAOF) writeEntryToBuffer(entry *WAL_Entry) error {
	marshaledEntry := MustMarshal(entry)

	size := int32(len(marshaledEntry))
	if err := binary.Write(wal.bufWriter, binary.LittleEndian, size); err != nil {
		return err
	}
	_, err := wal.bufWriter.Write(marshaledEntry)

	return err
}

// rotateLogIfNeeded is not thread safe
func (wal *WALAOF) rotateLogIfNeeded(entrySize int) error {
	if int64(wal.byteOffset+entrySize) > wal.maxSegmentSize {
		if err := wal.rotateLog(); err != nil {
			return err
		}
	}
	return nil
}

// rotateLog is not thread safe
func (wal *WALAOF) rotateLog() error {
	if err := wal.Sync(); err != nil {
		return err
	}

	if err := wal.currentSegmentFile.Close(); err != nil {
		return err
	}

	wal.currentSegmentIndex++

	if wal.currentSegmentIndex-wal.oldestSegmentIndex+1 > wal.maxSegmentCount {
		if err := wal.deleteOldestSegment(); err != nil {
			return err
		}
		wal.oldestSegmentIndex++
	}

	newFile, err := os.OpenFile(filepath.Join(wal.logDir, segmentPrefix+fmt.Sprintf("-%d", wal.currentSegmentIndex)), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		log.Fatalf("failed opening file: %s", err)
	}

	wal.byteOffset = 0

	wal.currentSegmentFile = newFile
	wal.bufWriter = bufio.NewWriter(newFile)

	return nil
}

func (wal *WALAOF) deleteOldestSegment() error {
	oldestSegmentFilePath := filepath.Join(wal.logDir, segmentPrefix+fmt.Sprintf("%d", wal.oldestSegmentIndex))

	// TODO: checkpoint before deleting the file

	if err := os.Remove(oldestSegmentFilePath); err != nil {
		return err
	}
	wal.oldestSegmentIndex++

	return nil
}

// Close the WAL file. It also calls Sync() on the WAL.
func (wal *WALAOF) Close() error {
	wal.cancel()
	if err := wal.Sync(); err != nil {
		return err
	}
	return wal.currentSegmentFile.Close()
}

// Writes out any data in the WAL's in-memory buffer to the segment file. If
// fsync is enabled, it also calls fsync on the segment file.
func (wal *WALAOF) Sync() error {
	if err := wal.bufWriter.Flush(); err != nil {
		return err
	}
	if wal.writeMode == "fsync" { //nolint:goconst
		if err := wal.currentSegmentFile.Sync(); err != nil {
			return err
		}
	}

	return nil
}

func (wal *WALAOF) keepSyncingBuffer() {
	for {
		select {
		case <-wal.bufferSyncTicker.C:

			wal.lock.Lock()
			err := wal.Sync()
			wal.lock.Unlock()

			if err != nil {
				log.Printf("Error while performing sync: %v", err)
			}

		case <-wal.ctx.Done():
			return
		}
	}
}

func (wal *WALAOF) rotateSegmentPeriodically() {
	for {
		select {
		case <-wal.segmentRotationTicker.C:

			wal.lock.Lock()
			err := wal.rotateLog()
			wal.lock.Unlock()
			if err != nil {
				log.Printf("Error while performing sync: %v", err)
			}

		case <-wal.ctx.Done():
			return
		}
	}
}

func (wal *WALAOF) deleteSegmentPeriodically() {
	for {
		select {
		case <-wal.segmentRetentionTicker.C:

			wal.lock.Lock()
			err := wal.deleteOldestSegment()
			wal.lock.Unlock()
			if err != nil {
				log.Printf("Error while deleting segment: %v", err)
			}
		case <-wal.ctx.Done():
			return
		}
	}
}

func (wal *WALAOF) ForEachCommand(f func(c cmd.DiceDBCmd) error) error {
	// TODO: implement this method
	return nil
}
