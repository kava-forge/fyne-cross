package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/klauspost/compress/zstd"

	archiver "github.com/mholt/archiver/v3"
)

type AWSSession struct {
	s      *session.Session
	bucket string

	m      sync.Mutex
	cancel context.CancelFunc
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return true
}

func NewAWSSessionFromEnvironment() (*AWSSession, error) {
	return NewAWSSession("", "", os.Getenv("AWS_S3_ENDPOINT"), os.Getenv("AWS_S3_REGION"), os.Getenv("AWS_S3_BUCKET"))
}

func NewAWSSession(akid string, secret string, endpoint string, region string, bucket string) (*AWSSession, error) {
	var cred *credentials.Credentials

	if akid != "" && secret != "" {
		cred = credentials.NewStaticCredentials(akid, secret, "")
	}

	s, err := session.NewSession(
		&aws.Config{
			Endpoint:    aws.String(endpoint),
			Region:      aws.String(region),
			Credentials: cred,
		},
	)
	if err != nil {
		return nil, err
	}

	return &AWSSession{s: s, bucket: bucket, cancel: func() {}}, nil
}

func (a *AWSSession) GetCredentials() (credentials.Value, error) {
	a.m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.m.Unlock()
	defer a.Cancel()

	return a.s.Config.Credentials.GetWithContext(ctx)
}

func (a *AWSSession) UploadFile(localFile string, s3FilePath string) error {
	file, err := os.Open(localFile)
	if err != nil {
		return err
	}
	defer file.Close()

	uploader := s3manager.NewUploader(a.s)

	a.m.Lock()
	ctxt, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.m.Unlock()
	defer a.Cancel()

	_, err = uploader.UploadWithContext(ctxt, &s3manager.UploadInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(s3FilePath),

		Body: file,
	})

	return err
}

func (a *AWSSession) UploadCompressedDirectory(localDirectoy string, s3FilePath string) error {
	in, out := io.Pipe()

	extension := strings.ToLower(filepath.Ext(s3FilePath))

	var compression archiver.Writer

	switch extension {
	case ".xz":
		compression = archiver.NewTarXz()
		err := compression.Create(out)
		if err != nil {
			return err
		}
	case ".zstd":
		inZstd, outTar := io.Pipe()

		compression = archiver.NewTar()
		err := compression.Create(outTar)
		if err != nil {
			return err
		}

		enc, err := zstd.NewWriter(out)
		if err != nil {
			return err
		}
		defer enc.Close()

		go func() {
			io.Copy(enc, inZstd)
		}()
	default:
		return fmt.Errorf("unknown extension for %v", s3FilePath)
	}

	errorChannel := make(chan error)

	go func() {
		err := filepath.Walk(localDirectoy, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			customName := strings.TrimPrefix(path, localDirectoy)
			if len(customName) == 0 {
				return fmt.Errorf("unexpected path: %v", path)
			}
			customName = filepath.ToSlash(customName)
			if customName[0] != '/' {
				customName = "/" + customName
			}

			return compression.Write(archiver.File{
				FileInfo: archiver.FileInfo{
					FileInfo:   info,
					CustomName: customName,
				},
				ReadCloser: f,
			})
		})

		compression.Close()

		errorChannel <- err
	}()

	uploader := s3manager.NewUploader(a.s)

	a.m.Lock()
	ctxt, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.m.Unlock()
	defer a.Cancel()

	_, err := uploader.UploadWithContext(ctxt, &s3manager.UploadInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(s3FilePath),

		Body: in,
	})
	if err != nil {
		return err
	}
	in.Close()

	err = <-errorChannel
	return err
}

func (a *AWSSession) DownloadFile(s3FilePath string, localFile string) error {
	f, err := os.Create(localFile)
	if err != nil {
		return err
	}

	downloader := s3manager.NewDownloader(a.s)

	a.m.Lock()
	ctxt, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.m.Unlock()
	defer a.Cancel()

	_, err = downloader.DownloadWithContext(ctxt, f, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(s3FilePath),
	})

	return err
}

func (a *AWSSession) DownloadCompressedDirectory(s3FilePath string, localRootDirectory string) error {
	in, out := io.Pipe()

	extension := strings.ToLower(filepath.Ext(s3FilePath))

	var compression archiver.Reader

	switch extension {
	case ".xz":
		compression = archiver.NewTarXz()
		err := compression.Open(in, 0)
		if err != nil {
			return err
		}
	case ".zstd":
		inTar, outZstd := io.Pipe()

		d, err := zstd.NewReader(in)
		if err != nil {
			return err
		}
		defer d.Close()

		// Copy content...
		_, err = io.Copy(outZstd, d)

		compression = archiver.NewTar()
		err = compression.Open(inTar, 0)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown extension for %v", s3FilePath)
	}

	errorChannel := make(chan error)

	go func() {
		var err error
		for {
			f, err := compression.Read()
			if err != nil {
				break
			}

			err = uncompressFile(localRootDirectory, f)
			if err != nil {
				break
			}
		}

		in.Close()
		if err == io.EOF {
			err = nil
		}

		errorChannel <- err
	}()

	downloader := s3manager.NewDownloader(a.s)
	downloader.Concurrency = 1

	a.m.Lock()
	ctxt, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.m.Unlock()
	defer a.Cancel()

	_, err := downloader.DownloadWithContext(ctxt, fakeWriterAt{out}, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(s3FilePath),
	})
	out.Close()
	if err != nil {
		return err
	}

	err = <-errorChannel
	return err

}

func (a *AWSSession) Cancel() {
	a.m.Lock()
	defer a.m.Unlock()

	a.cancel()
	a.cancel = func() {}
}

func uncompressFile(localRootDirectory string, f archiver.File) error {
	// be sure to close f before moving on!!
	defer f.Close()

	// Do not use strings.Split to split a path as it will generate empty string when given "//"
	splitFn := func(c rune) bool {
		return c == '/'
	}
	paths := strings.FieldsFunc(f.Name(), splitFn)

	if len(paths) == 0 {
		return fmt.Errorf("incorrect path")
	}

	// Replace top directory in the archive with local path
	paths[0] = localRootDirectory
	localFile := filepath.Join(paths...)

	if f.IsDir() {
		if !Exists(localFile) {
			log.Println("Creating directory:", localFile)
			return os.Mkdir(localFile, f.Mode().Perm())
		}
		return nil
	}

	outFile, err := os.Create(localFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	log.Println(f.Name(), "->", localFile)
	_, err = io.Copy(outFile, f)

	return err
}

func (a *AWSSession) GetBucket() string {
	return a.bucket
}

type fakeWriterAt struct {
	w io.Writer
}

func (fw fakeWriterAt) WriteAt(p []byte, offset int64) (n int, err error) {
	// ignore 'offset' because we forced sequential downloads
	return fw.w.Write(p)
}
