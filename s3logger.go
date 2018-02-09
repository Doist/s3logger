// TODO describe program
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/artyom/autoflags"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"golang.org/x/sync/errgroup"
)

func main() {
	args := runArgs{
		Dir:  "/var/spool/s3logger",
		D:    5 * time.Minute,
		Addr: "localhost:8080",
	}
	autoflags.Parse(&args)
	log := log.New(os.Stderr, "", log.LstdFlags)
	sess, err := session.NewSession()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		log.Print(<-sigCh)
		cancel()
	}()
	if err := run(ctx, args, log, s3manager.NewUploader(sess)); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	Addr   string        `flag:"addr,address to listen"`
	Bucket string        `flag:"bucket,s3 bucket to upload logs"`
	Dir    string        `flag:"dir,directory to keep unsent files"`
	D      time.Duration `flag:"t,time to use single file (min 1m)"`
}

func run(ctx context.Context, args runArgs, logger *log.Logger, upl *s3manager.Uploader) error {
	if args.Addr == "" {
		return errors.New("empty address")
	}
	if args.Bucket == "" {
		return errors.New("empty bucket name")
	}
	if args.Dir == "" {
		args.Dir = "."
	}
	if args.D < time.Minute {
		args.D = time.Minute
	}
	if err := os.MkdirAll(args.Dir, 0777); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", args.Addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	srv := &server{dir: args.Dir, ch: make(chan []byte, 100), log: logger}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { <-ctx.Done(); return ln.Close() })
	group.Go(func() error { return srv.ingest(ctx, args.D) })
	group.Go(func() error { return srv.upload(ctx, args.D, args.Bucket, upl) })
	group.Go(func() error {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
				return err
			}
			go func(conn net.Conn) {
				if err := srv.handleConn(conn); err != nil {
					logger.Printf("%s: %v", conn.RemoteAddr(), err)
				}
			}(conn)
		}
	})
	return group.Wait()
}

type server struct {
	dir string
	ch  chan []byte
	log *log.Logger

	mu   sync.Mutex
	w    io.WriteCloser
	name string
}

func (srv *server) upload(ctx context.Context, d time.Duration, bucket string, upl *s3manager.Uploader) error {
	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || path == srv.currentName() {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		start := time.Now()
		switch err := uploadFile(ctx, upl, bucket, path); err {
		case nil:
			srv.log.Printf("%q uploaded in %v", path, time.Since(start).Round(100*time.Millisecond))
			_ = os.Remove(path)
		default:
			srv.log.Printf("%q upload: %v", path, err)
		}
		return nil
	}

	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = filepath.Walk(srv.dir, walkFn)
		}
	}
}

func uploadFile(ctx context.Context, upl *s3manager.Uploader, bucket, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	key := strings.Replace(filepath.Base(name), "-", "/", 3) + ".json.gz"
	_, err = upl.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   f,
	})
	return err
}

func (srv *server) ingest(ctx context.Context, d time.Duration) error {
	var w io.Writer
	var err error
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case msg := <-srv.ch:
			if w == nil {
				if w, err = srv.create(); err != nil {
					srv.log.Print("file create: ", err)
					continue
				}
			}
			if _, err := w.Write(msg); err != nil {
				srv.log.Print("message write: ", err)
				srv.close()
				w = nil
			}
		case <-timer.C:
			timer.Reset(d)
			srv.close()
			w = nil
		case <-ctx.Done():
			return srv.close()
		}
	}
}

func (srv *server) create() (io.Writer, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	f, err := os.Create(filepath.Join(srv.dir, randomName()))
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	gw, err := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	if err != nil {
		panic(err)
	}
	srv.name, srv.w = f.Name(), chain{gw, bw, f}
	return srv.w, nil
}

func (srv *server) close() error {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.w == nil {
		return nil
	}
	w := srv.w
	srv.name, srv.w = "", nil
	return w.Close()
}

func (srv *server) currentName() string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.name
}

func (srv *server) handleConn(conn io.ReadCloser) error {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(3 * time.Minute)
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		var msg json.RawMessage
		switch err := dec.Decode(&msg); err {
		case nil:
		case io.EOF:
			return nil
		default:
			return err
		}
		select {
		case srv.ch <- msg:
		default:
			srv.log.Print("message dropped due to queue overflow")
		}
	}
}

// chain implements io.WriteCloser that passes writes to the first (0 index)
// Writer and on Close flushes and closes each Writer if they implement relevant
// interfaces.
type chain []io.Writer

func (cw chain) Write(p []byte) (int, error) { return cw[0].Write(p) }
func (cw chain) Close() error {
	var errOut error
	for _, w := range cw {
		if f, ok := w.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil && errOut == nil {
				errOut = err
			}
		}
		if c, ok := w.(io.Closer); ok {
			if err := c.Close(); err != nil && errOut == nil {
				errOut = err
			}
		}
	}
	return errOut
}

// randomName returns base name of the temporary file. It encodes file creation
// date and can be translated to s3 object name in date-sharded "subdirectories"
// by replacing - with /.
func randomName() string {
	const format = "2006-01-02-20060102T150405_"
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		panic(err)
	}
	b := make([]byte, len(format)+len(rnd)*2)
	hex.Encode(b[len(format):], rnd)
	time.Now().In(time.UTC).AppendFormat(b[:0], format)
	return string(b)
}
