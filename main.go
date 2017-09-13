package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var (
	gIsDebugMode = false
)

// StepParamsModel ...
type StepParamsModel struct {
	CacheAPIURL string
	IsDebugMode bool
}

// CreateStepParamsFromEnvs ...
func CreateStepParamsFromEnvs() (StepParamsModel, error) {
	stepParams := StepParamsModel{
		CacheAPIURL: os.Getenv("cache_api_url"),
		IsDebugMode: os.Getenv("is_debug_mode") == "true",
	}

	return stepParams, nil
}

func downloadCacheArchive(url string) error {
	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf(" [!] Failed to close Archive download response body: %s", err)
		}
	}()

	if resp.StatusCode != 200 {
		responseBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		return fmt.Errorf("Failed to download archive - non success response code: %d, body: %s", resp.StatusCode, string(responseBytes))
	}

	out, err := os.Create("/tmp/cache-archive.tar")
	if err != nil {
		return fmt.Errorf("Failed to open the local cache file for write: %s", err)
	}

	defer func() {
		if err := out.Close(); err != nil {
			log.Printf(" [!] Failed to close Archive download file: %+v", err)
		}
	}()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

func untarFiles(compressed bool) error {
	archiveFile, err := os.Open("/tmp/cache-archive.tar")
	if err != nil {
		return err
	}

	defer func() {
		if err := archiveFile.Close(); err != nil {
			log.Printf(" [!] Failed to close archive file: %+v", err)
		}
	}()

	var tarReader *tar.Reader

	if compressed {
		gzr, err := gzip.NewReader(archiveFile)
		if err != nil {
			return err
		}
		defer gzr.Close()

		tarReader = tar.NewReader(gzr)
	} else {
		tarReader = tar.NewReader(archiveFile)
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		if err := untarFile(tarReader, header); err != nil {
			return err
		}
	}

	return nil
}

// untarFile untars a single file from tr with header header into destination.
func untarFile(tr *tar.Reader, header *tar.Header) error {
	switch header.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(header.Name, 0755)
	case tar.TypeReg, tar.TypeRegA, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return writeNewFile(header, tr)
	case tar.TypeSymlink:
		return writeNewSymbolicLink(header)
	case tar.TypeLink:
		return writeNewHardLink(header)
	default:
		return fmt.Errorf("%s: unknown type flag: %c", header.Name, header.Typeflag)
	}
}

func writeNewFile(header *tar.Header, in io.Reader) error {
	fpath := header.Name
	fm := header.FileInfo().Mode()

	err := os.MkdirAll(filepath.Dir(fpath), 0755)
	if err != nil {
		return err
	}

	out, err := os.Create(fpath)
	if err != nil {
		return err
	}
	defer out.Close()

	err = out.Chmod(fm)
	if err != nil && runtime.GOOS != "windows" {
		return err
	}

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	err = os.Chtimes(fpath, header.ModTime, header.ModTime)
	if err != nil {
		return err
	}
	return nil
}

func writeNewSymbolicLink(header *tar.Header) error {
	err := os.MkdirAll(filepath.Dir(header.Name), 0755)
	if err != nil {
		return err
	}

	err = os.Symlink(header.Linkname, header.Name)
	if err != nil {
		return err
	}

	return nil
}

func writeNewHardLink(header *tar.Header) error {
	err := os.MkdirAll(filepath.Dir(header.Name), 0755)
	if err != nil {
		return err
	}

	err = os.Link(filepath.Join(header.Name, header.Linkname), header.Name)
	if err != nil {
		return err
	}

	return nil
}

// GenerateDownloadURLRespModel ...
type GenerateDownloadURLRespModel struct {
	DownloadURL string `json:"download_url"`
}

func getCacheDownloadURL(cacheAPIURL string) (string, error) {
	req, err := http.NewRequest("GET", cacheAPIURL, nil)
	if err != nil {
		return "", fmt.Errorf("Failed to create request: %s", err)
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Failed to send request: %s", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf(" [!] Exception: Failed to close response body, error: %s", err)
		}
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Request sent, but failed to read response body (http-code:%d): %s", resp.StatusCode, body)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 202 {
		return "", fmt.Errorf("Build cache not found. Probably cache not initialised yet (first cache push initialises the cache), nothing to worry about ;)")
	}

	var respModel GenerateDownloadURLRespModel
	if err := json.Unmarshal(body, &respModel); err != nil {
		return "", fmt.Errorf("Request sent, but failed to parse JSON response (http-code:%d): %s", resp.StatusCode, body)
	}

	if respModel.DownloadURL == "" {
		return "", fmt.Errorf("Request sent, but Download URL is empty (http-code:%d): %s", resp.StatusCode, body)
	}

	return respModel.DownloadURL, nil
}

func downloadFileWithRetry(cacheAPIURL string, localPath string) error {
	downloadURL, err := getCacheDownloadURL(cacheAPIURL)
	if err != nil {
		return err
	}
	if gIsDebugMode {
		log.Printf("   [DEBUG] downloadURL: %s", downloadURL)
	}

	if err := downloadCacheArchive(downloadURL); err != nil {
		fmt.Println()
		log.Printf(" ===> (!) First download attempt failed, retrying...")
		fmt.Println()
		return downloadCacheArchive(downloadURL)
	}
	return nil
}

func main() {
	log.Println("Cache pull...")

	stepParams, err := CreateStepParamsFromEnvs()
	if err != nil {
		log.Fatalf(" [!] Input error : %s", err)
	}
	gIsDebugMode = stepParams.IsDebugMode
	if gIsDebugMode {
		log.Printf("=> stepParams: %#v", stepParams)
	}
	if stepParams.CacheAPIURL == "" {
		log.Println(" (i) No Cache API URL specified, there's no cache to use, exiting.")
		return
	}

	//
	// Download Cache Archive
	//

	log.Println("=> Downloading Cache ...")
	startTime := time.Now()

	cacheArchiveFilePath := "/tmp/cache-archive.tar"
	if err := downloadFileWithRetry(stepParams.CacheAPIURL, cacheArchiveFilePath); err != nil {
		log.Fatalf(" [!] Unable to download cache: %s", err)
	}

	log.Println("=> Downloading Cache [DONE]")
	log.Println("=> Took: " + time.Now().Sub(startTime).String())

	log.Println("=> Uncompressing archive ...")
	startTime = time.Now()
	if err := untarFiles(false); err != nil {
		fmt.Println()
		log.Printf(" ===> (!) Uncompressing failed, retrying...")
		fmt.Println()
		err := untarFiles(true)
		if err != nil {
			log.Fatalf("Failed to uncompress archive, error: %+v", err)
		}
	}
	log.Println("=> Uncompressing archive [DONE]")
	log.Println("=> Took: " + time.Now().Sub(startTime).String())

	log.Println("=> Finished")
}
