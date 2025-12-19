package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	connectionTimeout = 30 * time.Second
	chunkSize         = 1024 * 32
	defaultPort       = "1977"
)

type FileInfo struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Chunks int    `json:"chunks"`
}

type FileChunk struct {
	Type   string `json:"type"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Data   []byte `json:"data"`
	IsLast bool   `json:"is_last"`
}

type ReceivedChunk struct {
	Data   []byte
	Index  int
	IsLast bool
}

func main() {
	var transferAddr string
	var receiveMode bool
	var outputDir string

	flag.StringVar(&transferAddr, "t", "", "Transfer file to address")
	flag.BoolVar(&receiveMode, "r", false, "Receive mode (listen for files)")
	flag.StringVar(&outputDir, "o", "./received", "Output directory for received files")
	flag.Parse()

	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:"+defaultPort, nil)
	if err != nil {
		fmt.Println("Error: Cannot connect to Nym client")
		os.Exit(1)
	}
	defer conn.Close()

	req, _ := json.Marshal(map[string]string{"type": "selfAddress"})
	conn.WriteMessage(websocket.TextMessage, req)

	var resp map[string]interface{}
	conn.ReadJSON(&resp)

	if receiveMode {
		runReceiveMode(conn, outputDir)
	} else if transferAddr != "" {
		if len(flag.Args()) != 1 {
			fmt.Println("Error: Exactly one file must be specified")
			fmt.Println("Usage: program -t <address> <file>")
			os.Exit(1)
		}
		runTransferMode(conn, transferAddr, flag.Arg(0))
	} else {
		fmt.Println("Use -r to receive files or -t <address> <file> to send a file")
	}
}

func runReceiveMode(conn *websocket.Conn, outputDir string) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Printf("Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Listening for files. Saving to: %s\n", outputDir)
	fmt.Printf("Press Ctrl+C to exit\n\n")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nExiting...")
		os.Exit(0)
	}()

	var currentFile *os.File
	var currentFileName string
	var currentFileSize int64
	var totalChunks int
	var nextExpectedIndex int
	var chunksWritten int
	var receivedChunks map[int]ReceivedChunk
	var lastDisplayedProgress int

	for {
		conn.SetReadDeadline(time.Time{})
		_, msg, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("Connection error: %v\n", err)
			break
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			continue
		}

		if data["type"] == "received" {
			message, _ := data["message"].(string)

			if len(message) == 0 {
				continue
			}

			var msgData map[string]interface{}
			if err := json.Unmarshal([]byte(message), &msgData); err != nil {
				handleRawData(message, outputDir, &currentFile,
					&currentFileName, &currentFileSize, &totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &lastDisplayedProgress)
				continue
			}

			msgType, _ := msgData["type"].(string)

			switch msgType {
			case "file_info":
				handleFileInfo(message, outputDir, &currentFile,
					&currentFileName, &currentFileSize, &totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &lastDisplayedProgress)

			case "file_chunk":
				handleFileChunkWithReorder(message, &currentFile,
					&currentFileName, currentFileSize, totalChunks,
					&nextExpectedIndex, &chunksWritten, &receivedChunks, &lastDisplayedProgress)
			}
		}
	}
}

func handleRawData(rawData, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, lastDisplayedProgress *int) {

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(rawData), &fileInfo); err == nil && fileInfo.Type == "file_info" {
		handleFileInfo(rawData, outputDir, currentFile, currentFileName,
			currentFileSize, totalChunks, nextExpectedIndex, chunksWritten, receivedChunks, lastDisplayedProgress)
		return
	}

	var chunk FileChunk
	if err := json.Unmarshal([]byte(rawData), &chunk); err == nil && chunk.Type == "file_chunk" {
		handleFileChunkWithReorder(rawData, currentFile, currentFileName,
			*currentFileSize, *totalChunks, nextExpectedIndex, chunksWritten, receivedChunks, lastDisplayedProgress)
		return
	}
}

func handleFileInfo(message, outputDir string, currentFile **os.File,
	currentFileName *string, currentFileSize *int64, totalChunks *int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, lastDisplayedProgress *int) {

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(message), &fileInfo); err != nil {
		return
	}

	if *currentFile != nil {
		(*currentFile).Close()
		*currentFile = nil
	}

	fmt.Printf("[Receiving] %s (%s, %d chunks)\n", 
		fileInfo.Name, formatBytes(fileInfo.Size), fileInfo.Chunks)

	// Reset state for new file
	*currentFileName = fileInfo.Name
	*currentFileSize = fileInfo.Size
	*totalChunks = fileInfo.Chunks
	*nextExpectedIndex = 0
	*chunksWritten = 0
	*lastDisplayedProgress = -1
	*receivedChunks = make(map[int]ReceivedChunk)

	filePath := filepath.Join(outputDir, fileInfo.Name)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		fmt.Printf("Error creating directory: %v\n", err)
		return
	}

	file, err := os.Create(filePath)
	if err != nil {
		fmt.Printf("Error creating file: %v\n", err)
		return
	}

	*currentFile = file
}

func handleFileChunkWithReorder(message string, currentFile **os.File,
	currentFileName *string, currentFileSize int64, totalChunks int,
	nextExpectedIndex, chunksWritten *int, receivedChunks *map[int]ReceivedChunk, lastDisplayedProgress *int) {

	if *currentFile == nil {
		return
	}

	var chunk FileChunk
	if err := json.Unmarshal([]byte(message), &chunk); err != nil {
		return
	}

	// Store chunk in map
	(*receivedChunks)[chunk.Index] = ReceivedChunk{
		Data:   chunk.Data,
		Index:  chunk.Index,
		IsLast: chunk.IsLast,
	}

	// Write chunks in correct order
	for {
		storedChunk, exists := (*receivedChunks)[*nextExpectedIndex]
		if !exists {
			break
		}

		if _, err := (*currentFile).Write(storedChunk.Data); err != nil {
			fmt.Printf("Error writing chunk %d: %v\n", *nextExpectedIndex, err)
			return
		}

		*chunksWritten++
		delete(*receivedChunks, *nextExpectedIndex)
		*nextExpectedIndex++

		// Calculate progress percentage
		currentProgress := int(float64(*chunksWritten) / float64(totalChunks) * 100)
		
		// Update display if progress changed
		if currentProgress != *lastDisplayedProgress {
			fmt.Printf("\r[Progress] %d%% (%d/%d)", 
				currentProgress, *chunksWritten, totalChunks)
			*lastDisplayedProgress = currentProgress
		}

		if storedChunk.IsLast || *chunksWritten == totalChunks {
			(*currentFile).Close()
			*currentFile = nil
			
			// Show 100% if not already shown
			if *chunksWritten != totalChunks || *lastDisplayedProgress != 100 {
				fmt.Printf("\r[Progress] 100%% (%d/%d)\n", totalChunks, totalChunks)
			} else {
				fmt.Println() // New line after progress display
			}
			
			if *chunksWritten == totalChunks {
				fmt.Printf("[Complete] %s (%s)\n\n",
					*currentFileName, formatBytes(currentFileSize))
			} else {
				fmt.Printf("[Warning] Incomplete: %s (%d/%d chunks)\n\n",
					*currentFileName, *chunksWritten, totalChunks)
			}
			
			*receivedChunks = nil
			break
		}
	}
}

func runTransferMode(conn *websocket.Conn, recipient string, filePath string) {
	fileInfo, err := getFileInfo(filePath)
	if err != nil {
		fmt.Printf("Error: %s - %v\n", filePath, err)
		os.Exit(1)
	}

	// Directly start with file info, no "Transferring to" line
	fmt.Printf("[Sending] %s (%s, %d chunks)\n",
		fileInfo.Name, formatBytes(fileInfo.Size), fileInfo.Chunks)

	fileInfo.Type = "file_info"
	if err := sendRawJSON(conn, recipient, fileInfo); err != nil {
		fmt.Printf("Error sending file info: %v\n", err)
		os.Exit(1)
	}

	time.Sleep(500 * time.Millisecond)

	if err := sendFileChunks(conn, recipient, filePath, fileInfo); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[Complete] %s (%s)\n", fileInfo.Name, formatBytes(fileInfo.Size))
}

func sendFileChunks(conn *websocket.Conn, recipient, filePath string, fileInfo FileInfo) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, chunkSize)
	chunkIndex := 0
	lastDisplayedProgress := -1

	for {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			return err
		}

		if n == 0 {
			break
		}

		chunk := FileChunk{
			Type:   "file_chunk",
			Index:  chunkIndex,
			Total:  fileInfo.Chunks,
			Data:   make([]byte, n),
			IsLast: err == io.EOF,
		}
		copy(chunk.Data, buffer[:n])

		if err := sendRawJSON(conn, recipient, chunk); err != nil {
			return err
		}

		// Calculate progress percentage
		currentProgress := int(float64(chunkIndex+1) / float64(fileInfo.Chunks) * 100)
		
		// Update display if progress changed
		if currentProgress != lastDisplayedProgress {
			fmt.Printf("\r[Progress] %d%% (%d/%d)", 
				currentProgress, chunkIndex+1, fileInfo.Chunks)
			lastDisplayedProgress = currentProgress
		}

		time.Sleep(10 * time.Millisecond)
		chunkIndex++
	}

	// Show 100% if not already shown
	if chunkIndex != fileInfo.Chunks || lastDisplayedProgress != 100 {
		fmt.Printf("\r[Progress] 100%% (%d/%d)\n", fileInfo.Chunks, fileInfo.Chunks)
	} else {
		fmt.Println() // New line after progress display
	}
	return nil
}

func getFileInfo(filePath string) (FileInfo, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return FileInfo{}, err
	}

	chunks := int((info.Size() + chunkSize - 1) / chunkSize)

	fileInfo := FileInfo{
		Name:   filepath.Base(filePath),
		Size:   info.Size(),
		Chunks: chunks,
	}

	return fileInfo, nil
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func sendRawJSON(conn *websocket.Conn, recipient string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	sendReq, err := json.Marshal(map[string]interface{}{
		"type":          "send",
		"recipient":     recipient,
		"message":       string(jsonData),
		"withReplySurb": false,
	})
	if err != nil {
		return err
	}

	conn.SetWriteDeadline(time.Now().Add(connectionTimeout))
	err = conn.WriteMessage(websocket.TextMessage, sendReq)
	conn.SetWriteDeadline(time.Time{})

	return err
}
