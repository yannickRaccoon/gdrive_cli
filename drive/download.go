package drive

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

type DownloadArgs struct {
	Out       io.Writer
	Progress  io.Writer
	Id        string
	Path      string
	Force     bool
	Skip      bool
	Recursive bool
	RecursiveExtraQuery string
	Delete    bool
	Stdout    bool
	Timeout   time.Duration
	Try       int
	IsAsyncDownload     bool
	LimitPerSec         int64
}

func (self *Drive) Download(args DownloadArgs) error {
	if args.Recursive {
		self.ResetDownloadTime()
		self.downloadRecursive(args)
		self.waitGroup.Wait()
		return self.downloadErr
	}

	f, err := self.service.Files.Get(args.Id).SupportsAllDrives(true).Fields("id", "name", "size", "mimeType", "md5Checksum").Do()
	if err != nil {
		if isBackendOrRateLimitError(err) && args.Try < MaxErrorRetries {
			exponentialBackoffSleep(args.Try)
			args.Try++
			return self.Download(args)
		}
 		return fmt.Errorf("Failed to get file: %s, err:", args.Id, err)
	}

	if isDir(f) {
		return fmt.Errorf("'%s' is a directory, use --recursive to download directories", f.Name)
	}

	if !isBinary(f) {
		return fmt.Errorf("'%s' is a google document and must be exported, see the export command", f.Name)
	}

	bytes, rate, err := self.downloadBinary(f, args)
	if err != nil {
		return err
	}

	if !args.Stdout {
		fmt.Fprintf(args.Out, "Downloaded %s at %s/s, total %s\n", f.Id, formatSize(rate, false), formatSize(bytes, false))
	}

	if args.Delete {
		err = self.deleteFile(args.Id)
		if err != nil {
			return fmt.Errorf("Failed to delete file: %s", err)
		}

		if !args.Stdout {
			fmt.Fprintf(args.Out, "Removed %s\n", args.Id)
		}
	}
	return err
}

type DownloadQueryArgs struct {
	Out       io.Writer
	Progress  io.Writer
	Query     string
	Path      string
	Force     bool
	Skip      bool
	Recursive bool
	RecursiveExtraQuery string
	Try       int
	IsAsyncDownload     bool
	LimitPerSec         int64
}

func (self *Drive) DownloadQuery(args DownloadQueryArgs) error {
	listArgs := listAllFilesArgs{
		query:  args.Query,
		fields: []googleapi.Field{"nextPageToken", "files(id,name,mimeType,size,md5Checksum)"},
	}
	files, err := self.listAllFiles(listArgs)
	if err != nil {
		if isBackendOrRateLimitError(err) && args.Try < MaxErrorRetries {
			exponentialBackoffSleep(args.Try)
			args.Try++
			return self.DownloadQuery(args)
		}
		return fmt.Errorf("Failed to list files: %s", err)
	}

	downloadArgs := DownloadArgs{
		Out:      args.Out,
		Progress: args.Progress,
		Path:     args.Path,
		Force:    args.Force,
		Skip:     args.Skip,
		RecursiveExtraQuery: args.RecursiveExtraQuery,
		IsAsyncDownload:     args.IsAsyncDownload,
		LimitPerSec:         args.LimitPerSec,
	}

	self.ResetDownloadTime()

	for _, f := range files {
		if isDir(f) && args.Recursive {
			err = self.downloadDirectory(f, downloadArgs)
		} else if isBinary(f) {
			_, _, err = self.downloadBinary(f, downloadArgs)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (self *Drive) downloadRecursive(args DownloadArgs) error {
	self.downloadCount++
	period := time.Now().Unix() - self.downloadStartUnix
	if period < 1 {
		period = 1
	}
	limit := period * args.LimitPerSec
	if limit < 0 {
		limit = math.MaxInt64
	}
	for self.downloadCount > limit {
		gap := self.downloadCount - limit
		time.Sleep(time.Duration(gap) * time.Second)
		period = time.Now().Unix() - self.downloadStartUnix
		limit = period * args.LimitPerSec
	}

	f, err := self.service.Files.Get(args.Id).SupportsAllDrives(true).Fields("id", "name", "size", "mimeType", "md5Checksum").Do()
	if err != nil {
		if isBackendOrRateLimitError(err) && args.Try < MaxErrorRetries {
			exponentialBackoffSleep(args.Try)
			args.Try++
			return self.downloadRecursive(args)
		}
		return fmt.Errorf("Failed to get file: %s, err:", args.Id, err)
	}

	if args.IsAsyncDownload {
		self.waitGroup.Add(1)
		go func() {
			self.doDownloadRecursive(f, args)
			self.waitGroup.Done()
		}()
	} else {
		self.doDownloadRecursive(f, args)
	}

	return nil
}

func (self *Drive) doDownloadRecursive(f *drive.File, args DownloadArgs) {
	var err error
	if isDir(f) {
		err = self.downloadDirectory(f, args)
	} else if isBinary(f) {
		_, _, err = self.downloadBinary(f, args)
	}

	if err != nil {
		fmt.Errorf("%s \n %s", self.downloadErr, err.Error())
		//fmt.Println("Failed to download:", err)
	}
}

func (self *Drive) downloadBinary(f *drive.File, args DownloadArgs) (int64, int64, error) {
	// Get timeout reader wrapper and context
	timeoutReaderWrapper, ctx := getTimeoutReaderWrapperContext(args.Timeout)

	res, err := self.service.Files.Get(f.Id).SupportsAllDrives(true).Context(ctx).Download()
	if err != nil {
		if isTimeoutError(err) {
			return 0, 0, fmt.Errorf("Failed to download file: timeout, no data was transferred for %v", args.Timeout)
		} else if isBackendOrRateLimitError(err) && (args.Try < MaxErrorRetries) {
			exponentialBackoffSleep(args.Try)
			args.Try++
			return self.downloadBinary(f, args)
		}
		return 0, 0, fmt.Errorf("Failed to download file: %s", err)
	}

	// Close body on function exit
	defer res.Body.Close()

	// Path to file
	fpath := filepath.Join(args.Path, f.Name)


	if !args.Stdout {
		fmt.Fprintf(args.Out, "Downloading %s -> %s\n", f.Name, fpath)
	}

	return self.saveFile(saveFileArgs{
		out:           args.Out,
		body:          timeoutReaderWrapper(res.Body),
		contentLength: res.ContentLength,
		fpath:         fpath,
		force:         args.Force,
		skip:          args.Skip,
		stdout:        args.Stdout,
		progress:      args.Progress,
	})
}

type saveFileArgs struct {
	out           io.Writer
	body          io.Reader
	contentLength int64
	fpath         string
	force         bool
	skip          bool
	stdout        bool
	progress      io.Writer
}

func (self *Drive) saveFile(args saveFileArgs) (int64, int64, error) {
	// Wrap response body in progress reader
	srcReader := getProgressReader(args.body, args.progress, args.contentLength)

	if args.stdout {
		// Write file content to stdout
		_, err := io.Copy(args.out, srcReader)
		return 0, 0, err
	}

	// Check if file exists to force
	if !args.skip && !args.force && fileExists(args.fpath) {
		return 0, 0, fmt.Errorf("File '%s' already exists, use --force to overwrite or --skip to skip", args.fpath)
	}

	//Check if file exists to skip
	if args.skip && fileExists(args.fpath) {
		fmt.Printf("File '%s' already exists, skipping\n", args.fpath)
		return 0, 0, nil
	}

	// Ensure any parent directories exists
	if err := mkdir(args.fpath); err != nil {
		return 0, 0, err
	}

	// Download to tmp file
	tmpPath := args.fpath + ".incomplete"

	// Create new file
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return 0, 0, fmt.Errorf("Unable to create new file: %s", err)
	}

	started := time.Now()

	// Save file to disk
	bytes, err := io.Copy(outFile, srcReader)
	if err != nil {
		outFile.Close()
		os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("Failed saving file: %s", err)
	}

	// Calculate average download rate
	rate := calcRate(bytes, started, time.Now())

	// Close File
	outFile.Close()

	// Rename tmp file to proper filename
	return bytes, rate, os.Rename(tmpPath, args.fpath)
}

func (self *Drive) downloadDirectory(parent *drive.File, args DownloadArgs) error {
	listArgs := listAllFilesArgs{
		query:  fmt.Sprintf("'%s' in parents %s", parent.Id, args.RecursiveExtraQuery),
		fields: []googleapi.Field{"nextPageToken", "files(id,name)"},
	}
	files, err := self.listAllFiles(listArgs)
	if err != nil {
		if isBackendOrRateLimitError(err) && args.Try < MaxErrorRetries {
			exponentialBackoffSleep(args.Try)
			args.Try++
			return self.downloadDirectory(parent, args)
		}
		return fmt.Errorf("Failed listing files: %s", err)
	}

	newPath := filepath.Join(args.Path, parent.Name)

	for _, f := range files {
		// Copy args and update changed fields
		newArgs := args
		newArgs.Path = newPath
		newArgs.Id = f.Id
		newArgs.Stdout = false

		err = self.downloadRecursive(newArgs)
		if err != nil {
			return err
		}
	}

	return nil
}

func isDir(f *drive.File) bool {
	return f.MimeType == DirectoryMimeType
}

func isBinary(f *drive.File) bool {
	return f.Md5Checksum != ""
}
