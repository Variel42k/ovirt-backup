package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
)

const maxRange = 32 << 20

var ticketPath = regexp.MustCompile(`^/images/[[:xdigit:]]{8}(-[[:xdigit:]]{4}){3}-[[:xdigit:]]{12}$`)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// Сеанс Engine ведёт скрипт. Утилита получает только временный билет
// доступа к данным через stdin, поэтому его нет в аргументах процесса.
func readTicket(input io.Reader) (*url.URL, error) {
	reader := bufio.NewReaderSize(input, 4096)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, errors.New("expected one ImageIO HTTPS ticket URL on stdin, terminated by newline")
	}
	endpoint, err := url.Parse(strings.TrimSpace(string(line)))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.Port() != "54322" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" ||
		!ticketPath.MatchString(endpoint.EscapedPath()) {
		return nil, errors.New("expected a direct HTTPS ImageIO URL on port 54322")
	}
	return endpoint, nil
}

func dataClient(roots *x509.CertPool, offset, length int64) *http.Client {
	transport := &http.Transport{
		DialContext:     (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(request)
			if err != nil || request.Method != http.MethodGet || response.StatusCode >= 300 {
				return response, err
			}
			// Принимаем только чанк заданного размера с запрошенного смещения.
			// Ответ с целым диском или с другим смещением отклоняем.
			prefix := fmt.Sprintf("bytes %d-%d/", offset, offset+length-1)
			if response.StatusCode != http.StatusPartialContent || response.ContentLength != length ||
				!strings.HasPrefix(response.Header.Get("Content-Range"), prefix) {
				response.Body.Close()
				return nil, errors.New("unexpected ImageIO range response")
			}
			return response, nil
		}),
	}
}

func transfer(ctx context.Context, client *imageio.Client, direction, path string, offset, length int64, output io.Writer) error {
	if direction == "download" {
		_, err := client.ReadRange(ctx, offset, length, output)
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || offset > info.Size() || length > info.Size()-offset {
		return errors.New("the requested range is outside the regular source file")
	}
	// Сохраняем flush для совместимости с демоном версии 4.3.
	// Engine завершает передачу и меняет подключения дисков после проверки чтением.
	return client.WriteRange(ctx, offset, io.NewSectionReader(file, offset, length), length, true)
}

func run(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("jhvirt-imageio-range", flag.ContinueOnError)
	direction := flags.String("direction", "", "upload or download")
	path := flags.String("file", "", "source image for upload")
	ca := flags.String("ca", "", "trusted target Engine CA (PEM)")
	offset := flags.Int64("offset", -1, "zero-based offset")
	length := flags.Int64("length", 0, "length, at most 32 MiB")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *ca == "" || *offset < 0 || *length <= 0 || *length > maxRange ||
		*offset > (1<<63-1)-*length ||
		(*direction != "download" && (*direction != "upload" || *path == "")) {
		return errors.New("invalid direction, CA, file or range; use --help")
	}
	endpoint, err := readTicket(input)
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(*ca)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("the CA file contains no certificate")
	}
	client := imageio.New(endpoint.String(), dataClient(roots, *offset, *length))
	if err := transfer(context.Background(), client, *direction, *path, *offset, *length, output); err != nil {
		var apiError *imageio.Error
		if errors.As(err, &apiError) {
			return fmt.Errorf("ImageIO %s: HTTP %d", *direction, apiError.Status)
		}
		return errors.New(strings.ReplaceAll(err.Error(), strings.TrimPrefix(endpoint.Path, "/images/"), "[ticket]"))
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
