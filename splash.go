package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{}
var chunkCache = make(map[string][]byte)
var chunkParentCount = make(map[string]int)
var cacheLock sync.Mutex

// Flags
var (
	platform           string
	manifestID         string
	manifestPath       string
	installPath        string
	chunkPath          string
	onlyDLChunks       bool
	fileFilter         map[string]bool = make(map[string]bool)
	downloadURLs       []string
	skipIntegrityCheck bool
	workerCount        int
)

const defaultDownloadURL = "http://epicgames-download1.akamaized.net"

func init() {
	// Seed random
	rand.Seed(time.Now().Unix())

	// Parse flags
	flag.StringVar(&platform, "platform", "Windows", "platform to download for")
	//flag.StringVar(&manifestID, "manifest", "", "download a specific manifest")
	flag.StringVar(&manifestPath, "manifest-file", "", "download specific manifest(s) - comma-separated list")
	flag.StringVar(&installPath, "install-dir", "", "folder to write downloaded files to")
	flag.StringVar(&chunkPath, "chunk-dir", "", "folder to read predownloaded chunks from")
	flag.BoolVar(&onlyDLChunks, "chunks-only", false, "only download chunks")
	dlFilter := flag.String("files", "", "comma-separated list of files to download")
	dlUrls := flag.String("url", defaultDownloadURL, "download url")
	httpTimeout := flag.Int64("http-timeout", 60, "http timeout in seconds")
	flag.BoolVar(&skipIntegrityCheck, "skipcheck", false, "skip file integrity check")
	flag.IntVar(&workerCount, "workers", 10, "amount of workers")
	flag.Parse()

	if manifestPath == "" {
		manifestPath = flag.Arg(0)
	}

	for _, file := range strings.Split(*dlFilter, ",") {
		fileFilter[file] = true
	}

	downloadURLs = strings.Split(*dlUrls, ",")
	httpClient.Timeout = time.Duration(*httpTimeout) * time.Second
}

func main() {
	var catalog *Catalog
	manifests := make([]*Manifest, 0)

	// Load catalog
	if manifestID == "" && manifestPath == "" {
		// Fetch latest catalog
		log.Println("Fetching latest catalog...")

		// Fetch from MCP
		catalogBytes, err := fetchCatalog(platform, "fn", "4fe75bbc5a674f4f9b356b5c90567da5", "Fortnite", "Live")
		if err != nil {
			log.Fatalf("Failed to fetch catalog: %v", err)
		}

		// Parse data
		catalog, err = parseCatalog(catalogBytes)
		if err != nil {
			log.Fatalf("Failed to parse catalog: %v", err)
		}

		// Sanity check catalog
		if len(catalog.Elements) != 1 || len(catalog.Elements[0].Manifests) < 1 {
			log.Fatal("Unsupported catalog")
		}

		log.Printf("Catalog %s (%s) %s loaded.\n", catalog.Elements[0].AppName, catalog.Elements[0].LabelName, catalog.Elements[0].BuildVersion)
	}

	// Load manifest
	if manifestID != "" { // fetch specific manifest
		log.Printf("Fetching manifest %s...", manifestID)

		manifest, _, err := fetchManifest(fmt.Sprintf("%s/Builds/Fortnite/CloudDir/%s.manifest", defaultDownloadURL, manifestID))
		if err != nil {
			log.Fatalf("Failed to fetch manifest: %v", err)
		}
		manifests = append(manifests, manifest)
	} else if manifestPath != "" { // read manifest(s) from disk
		for _, manifestPath := range strings.Split(manifestPath, ",") {
			manifest, err := readManifestFile(manifestPath)
			if err != nil {
				log.Fatalf("Failed to read manifest %s: %v", manifestPath, err)
			}

			log.Printf("Manifest %s %s loaded.\n", manifest.AppNameString, manifest.BuildVersionString)

			manifests = append(manifests, manifest)
		}
	} else { // otherwise, fetch from catalog
		log.Println("Fetching latest manifest...")

		manifest, _, err := fetchManifest(catalog.GetManifestURL())
		if err != nil {
			log.Fatalf("Failed to fetch manifest: %v", err)
		}
		manifests = append(manifests, manifest)
	}

	manifestFiles := make(map[string]ManifestFile)
	manifestChunks := make(map[string]Chunk)
	checkedFiles := make(map[string]ManifestFile)

	// Parse manifests
	for _, manifest := range manifests {
		for _, file := range manifest.FileManifestList {
			// Check filter
			if _, ok := fileFilter[file.FileName]; !ok && len(fileFilter) > 0 {
				continue
			}

			// Set full file path
			file.FileName = filepath.Join(installPath, strings.TrimSuffix(strings.TrimPrefix(manifest.BuildVersionString, "++Fortnite+Release-"), "-"+platform), file.FileName)

			// Add file
			manifestFiles[file.FileName] = file

			// Add all chunks
			for _, c := range file.FileChunkParts {
				chunkParentCount[c.GUID]++

				if _, ok := manifestChunks[c.GUID]; !ok { // don't add duplicates
					manifestChunks[c.GUID] = NewChunk(c.GUID, manifest.ChunkHashList[c.GUID], manifest.ChunkShaList[c.GUID], manifest.DataGroupList[c.GUID], manifest.ChunkFilesizeList[c.GUID])
				}
			}
		}
	}

	log.Printf("Downloading %d files in %d chunks from %d manifests.\n", len(manifestFiles), len(manifestChunks), len(manifests))

	// Hacky hacks for chunk-only download
	if onlyDLChunks {
		manifestFiles = make(map[string]ManifestFile)
		for k, chunk := range manifestChunks {
			manifestFiles[k] = ManifestFile{FileName: filepath.Join(installPath, chunk.GUID), FileHash: chunk.Hash, FileChunkParts: []ManifestFileChunkPart{{GUID: chunk.GUID, Offset: "000000000000", Size: chunk.OriginalSize}}}
		}
	}

	// Download and assemble files
	for k, file := range manifestFiles {
		func() {
			filePath := file.FileName

			// Check if file already exists
			if _, err := os.Stat(filePath); err == nil {
				// Open file
				diskFile, err := os.Open(filePath)
				if err == nil {
					// Calculate checksum
					hasher := sha1.New()
					_, err := io.Copy(hasher, diskFile)
					diskFile.Close()

					if err == nil {
						// Compare checksum
						if bytes.Equal(hasher.Sum(nil), readPackedData(file.FileHash)) {
							// Remove any trailing chunks
							for _, chunkPart := range file.FileChunkParts {
								chunkUsed(chunkPart.GUID)
							}

							log.Printf("File %s found on disk!\n", file.FileName)
							checkedFiles[k] = file
							return
						}
					}
				}
			}

			log.Printf("Downloading %s from %d chunks...\n", file.FileName, len(file.FileChunkParts))

			// Create outfile
			os.MkdirAll(filepath.Dir(filePath), os.ModePerm)
			outFile, err := os.Create(filePath)
			if err != nil {
				log.Printf("Failed to create %s: %v\n", filePath, err)
				return
			}
			defer outFile.Close()

			// Parse chunk parts
			chunkPartCount := len(file.FileChunkParts)
			chunkJobs := make([]ChunkJob, chunkPartCount)
			jobs := make(chan ChunkJob, chunkPartCount)
			for i, chunkPart := range file.FileChunkParts {
				chunkJobs[i] = ChunkJob{ID: i, Chunk: manifestChunks[chunkPart.GUID], Part: ChunkPart{Offset: readPackedUint32(chunkPart.Offset), Size: readPackedUint32(chunkPart.Size)}}

				// Check if present on disk
				if chunkPath != "" {
					localPath := filepath.Join(chunkPath, chunkPart.GUID)
					if _, err := os.Stat(localPath); err == nil {
						chunkJobs[i].LocalPath = localPath
					}
				}

				jobs <- chunkJobs[i]
			}

			results := make(chan ChunkJobResult, chunkPartCount)
			orderedResults := make(chan ChunkJobResult, chunkPartCount)

			// Order results as they come in
			go func() {
				resultsBuffer := make(map[int]ChunkJobResult)
				for result := range results {
					resultsBuffer[result.Job.ID] = result

				loop:
					if len(chunkJobs) > 0 {
						if res, ok := resultsBuffer[chunkJobs[0].ID]; ok {
							orderedResults <- res
							chunkJobs = chunkJobs[1:]
							delete(resultsBuffer, res.Job.ID)
							goto loop
						}
					}
				}
			}()

			// Spawn workers
			for i := 0; i < workerCount; i++ {
				go chunkWorker(jobs, results)
			}

			// Handle results
			for i := 0; i < chunkPartCount; i++ {
				result := <-orderedResults

				// Write chunk part to file
				result.Reader.Seek(int64(result.Job.Part.Offset), io.SeekCurrent)
				_, err := io.CopyN(outFile, result.Reader, int64(result.Job.Part.Size))

				// Close reader
				result.Reader.Close()

				if err != nil {
					log.Printf("Failed to write chunk %s to file %s: %v\n", result.Job.Chunk.GUID, file.FileName, err)
					continue
				}
			}
			close(jobs)
			close(results)
		}()
	}

	// Integrity check
	if !skipIntegrityCheck {
		log.Println("Verifying file integrity...")

		for k, file := range manifestFiles {
			// Skip prechecked files
			if _, ok := checkedFiles[k]; ok {
				continue
			}

			// Open file
			f, err := os.Open(file.FileName)
			if err != nil {
				log.Printf("Failed to open %s: %v\n", file.FileName, err)
				continue
			}

			// Hash file
			hasher := sha1.New()
			_, err = io.Copy(hasher, f)
			f.Close()

			if err != nil {
				log.Printf("Failed to hash %s: %v\n", file.FileName, err)
				continue
			}

			// Compare checksum
			expectedHash := readPackedData(file.FileHash)
			actualHash := hasher.Sum(nil)
			if !bytes.Equal(actualHash, expectedHash) {
				log.Printf("File %s is corrupt - got hash %s but want %s\n", file.FileName, hex.EncodeToString(actualHash), hex.EncodeToString(expectedHash))
			}
		}
	}

	log.Println("Done!")
}

func chunkUsed(guid string) {
	// Chunk was used once
	chunkParentCount[guid]--

	// Check if we still need to store chunk in cache
	if chunkParentCount[guid] < 1 {
		delete(chunkCache, guid)
	}
}

func chunkWorker(jobs chan ChunkJob, results chan<- ChunkJobResult) {
	for j := range jobs {
		var chunkReader ReadSeekCloser
		cacheLock.Lock()
		if _, ok := chunkCache[j.Chunk.GUID]; ok {
			// Read from cache
			chunkReader = NewByteCloser(chunkCache[j.Chunk.GUID])

			cacheLock.Unlock()
		} else if j.LocalPath != "" {
			cacheLock.Unlock()

			var err error
			chunkReader, err = os.Open(j.LocalPath)
			if err != nil {
				log.Printf("Failed to open chunk %s from disk: %v\n", j.Chunk.GUID, err)
				jobs <- j
				continue
			}
		} else {
			cacheLock.Unlock()

			// Download chunk
			chunkData, err := j.Chunk.Download(downloadURLs[rand.Intn(len(downloadURLs))])
			if err != nil {
				log.Printf("Failed to download chunk %s: %v\n", j.Chunk.GUID, err)
				jobs <- j // requeue
				continue
			}

			// Create new reader
			chunkReader = NewByteCloser(chunkData)

			// Return raw chunk if not downloading full files, also skip any caching
			if onlyDLChunks {
				results <- ChunkJobResult{Job: j, Reader: chunkReader}
				continue
			}

			// Read chunk header
			chunkHeader, err := readChunkHeader(chunkReader)
			if err != nil {
				log.Printf("Failed to read chunk header %s: %v\n", j.Chunk.GUID, err)
				jobs <- j
				continue
			}

			// Decompress if needed
			if chunkHeader.StoredAs == 1 {
				// Create decompressor
				zlibReader, err := zlib.NewReader(chunkReader)
				if err != nil {
					log.Printf("Failed to create decompressor for chunk %s: %v\n", j.Chunk.GUID, err)
					jobs <- j
					continue
				}

				// Decompress entire chunk
				chunkData, err = ioutil.ReadAll(zlibReader)
				if err != nil {
					zlibReader.Close()

					log.Printf("Failed to decompress chunk %s: %v\n", j.Chunk.GUID, err)
					jobs <- j
					continue
				}
				zlibReader.Close()

				// Set reader to decompressed data
				chunkReader = NewByteCloser(chunkData)
			} else if chunkHeader.StoredAs != 0 {
				log.Printf("Got unknown chunk (storedas: %d) %s\n", chunkHeader.StoredAs, j.Chunk.GUID)
				jobs <- j
				continue
			}

			// Store in cache if needed later
			cacheLock.Lock()
			if chunkParentCount[j.Chunk.GUID] > 1 {
				if chunkHeader.StoredAs == 0 { // chunkData still contains header here
					chunkCache[j.Chunk.GUID] = chunkData[62:]
				} else {
					chunkCache[j.Chunk.GUID] = chunkData
				}
			}
			cacheLock.Unlock()
		}

		// Chunk was used once
		cacheLock.Lock()
		chunkUsed(j.Chunk.GUID)
		cacheLock.Unlock()

		// Pass result
		results <- ChunkJobResult{Job: j, Reader: chunkReader}
	}
}
