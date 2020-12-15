package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestClient(t *testing.T) {
	ctx := context.Background()
	// HTTPS
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, TCPProbe")
	}))

	r := request{
		count:    2,
		quiet:    true,
		timeout:  time.Second * 2,
		insecure: true,
	}

	c := newClient(&r, ts.URL)
	assert.Equal(t, 2, c.req.count)

	err := c.connect(ctx)
	assert.NoError(t, err)
	err = c.httpGet()
	assert.NoError(t, err)
	err = c.getTCPInfo()
	assert.NoError(t, err)
	c.close()

	c = newClient(&r, ts.URL)
	c.probe(ctx)

	assert.Equal(t, uint8(1), c.stats.State)
	assert.Equal(t, 200, c.HTTPStatusCode)
	assert.Equal(t, int64(16), c.stats.HTTPRcvdBytes)
	assert.Equal(t, int64(0), c.stats.TCPConnectError)
	assert.Equal(t, int64(0), c.stats.DNSResolveError)
	assert.Equal(t, uint32(0), c.stats.Unacked)
	assert.Equal(t, uint32(0), c.stats.Lost)
	assert.Less(t, uint32(0), c.stats.Rto)
	assert.Less(t, uint32(0), c.stats.Ato)
	assert.Less(t, int64(0), c.stats.TLSHandshake)

	c.close()

	// HTTP
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, TCPProbe")
	}))

	c = newClient(&r, ts.URL)
	err = c.connect(ctx)
	assert.NoError(t, err)
	err = c.httpGet()
	assert.NoError(t, err)
	err = c.getTCPInfo()
	assert.NoError(t, err)

	assert.Equal(t, int64(0), c.stats.TLSHandshake)

	c.close()

	// unreachable host
	c = newClient(&r, "127.0.0.0")
	err = c.connect(ctx)
	assert.Error(t, err)

	// name not known
	c = newClient(&r, "tcpprobeunknowndomain.com")
	err = c.connect(ctx)
	assert.Error(t, err)

	// unreachable ipv6 addr
	c = newClient(&r, "[::1]:5050")
	err = c.connect(ctx)
	assert.Error(t, err)

	// wrong target
	c = newClient(&r, ":::")
	err = c.connect(ctx)
	assert.Error(t, err)

	// external, without explicit port
	c = newClient(&r, "https://www.google.com")
	err = c.connect(ctx)
	assert.NoError(t, err)
	c.close()
}

func TestCli(t *testing.T) {
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	args := []string{"tcpprobe", "-metrics"}
	_, m, err := getCli(args)
	assert.NoError(t, err)
	assert.Len(t, m, 0)
	buf := new(bytes.Buffer)
	io.CopyN(buf, r, 7)
	assert.Equal(t, "metrics", buf.String())

	r, w, _ = os.Pipe()
	os.Stdout = w
	args = []string{"tcpprobe"}
	_, m, err = getCli(args)
	assert.Error(t, err)
	assert.Len(t, m, 0)
	buf.Reset()

	io.CopyN(buf, r, 5)
	assert.Equal(t, "usage", buf.String())

	args = []string{"tcpprobe", "127.0.0.1"}
	_, m, err = getCli(args)
	assert.NoError(t, err)
	assert.Len(t, m, 1)

	os.Stdout = stdout
}

func TestPrometheus(t *testing.T) {
	c := &client{}
	c.prometheus(context.Background())

	v := reflect.ValueOf(&c.stats).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)

		if f.Tag.Get("unexported") == "true" {
			continue
		}

		req := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "tp_" + f.Tag.Get("name"),
			Help:        f.Tag.Get("help"),
			ConstLabels: prometheus.Labels{"target": c.target},
		})

		if err := prometheus.Register(req); err != nil {
			_, ok := err.(prometheus.AlreadyRegisteredError)
			assert.True(t, ok)
		}
	}
}

func TestServerName(t *testing.T) {
	r := request{
		serverName: "myserver",
	}

	c := newClient(&r, "target")
	assert.Equal(t, "myserver", c.serverName())

	c = newClient(&request{}, "target")
	assert.Equal(t, "target", c.serverName())
}

func TestGetSrcAddr(t *testing.T) {
	addr := getSrcAddr("")
	assert.Nil(t, addr)

	addr = getSrcAddr("192.168.1.1")
	assert.Equal(t, &net.TCPAddr{
		IP:   net.ParseIP("192.168.1.1"),
		Port: 0, Zone: "",
	}, addr)
}

func TestPrintText(t *testing.T) {
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	c := &client{stats: stats{Rtt: 5}, req: &request{filter: "rtt"}}
	c.printer(0)

	buf := new(bytes.Buffer)
	io.CopyN(buf, r, 46)
	assert.Contains(t, buf.String(), "Rtt:5")

	os.Stdout = stdout
}

func TestPrintJsonPretty(t *testing.T) {
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	c := &client{stats: stats{}, req: &request{jsonPretty: true, filter: "rtt"}}
	c.printer(0)

	buf := make([]byte, 13)
	n, _ := io.ReadFull(r, buf)
	assert.Equal(t, 13, n)
	assert.Equal(t, "{\n \"Rtt\": 0\n}", string(buf))

	os.Stdout = stdout
}

func TestPrintJson(t *testing.T) {
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	c := &client{stats: stats{}, req: &request{json: true, filter: "rtt"}}
	c.printer(0)

	buf := make([]byte, 9)
	n, _ := io.ReadFull(r, buf)
	assert.Equal(t, 9, n)
	assert.Equal(t, `{"Rtt":0}`, string(buf))

	os.Stdout = stdout
}

func TestBoolToInt(t *testing.T) {
	assert.Equal(t, 1, boolToInt(true))
	assert.Equal(t, 0, boolToInt(false))
}

func TestNoRedirect(t *testing.T) {
	c := &client{}
	assert.Error(t, c.noRedirect(nil, nil))
}

func TestMain(t *testing.T) {
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, TCPProbe")
	}))
	os.Args = []string{"tcpprobe", "-c", "1", "-insecure", ts.URL}
	main()

	buf := new(bytes.Buffer)
	io.CopyN(buf, r, 800)

	assert.Contains(t, buf.String(), "target: https://127.0.0.1")
	assert.Contains(t, buf.String(), "HTTPStatusCode:200")

	os.Stdout = stdout
}

func TestGetLabels(t *testing.T) {
	labels := map[string]string{"key": "value"}
	b, _ := json.Marshal(labels)
	ctx := context.WithValue(context.Background(), labelsKey, b)
	l := getLabels(ctx, "127.0.0.1")
	assert.Contains(t, l, "key")
	assert.Contains(t, l, "target")

	ctx = context.WithValue(context.Background(), labelsKey, []byte(""))
	getLabels(ctx, "127.0.0.1")
	assert.Contains(t, l, "target")
}

func TestK8SStart(t *testing.T) {
	ctx := context.Background()
	tp := &tp{targets: make(map[string]prop)}
	req := &request{namespace: "default"}

	samplePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fake",
			Namespace: "default",
			Annotations: map[string]string{
				"tcpprobe/targets":  "https://www.google.com",
				"tcpprobe/interval": "6s",
				"tcpprobe/labels":   "{\"mykey\":\"myvalue\"}",
			},
		},
		Status: v1.PodStatus{Phase: "Running"},
	}

	clientset := fake.NewSimpleClientset(samplePod)
	k := k8s{clientset: clientset, pods: sync.Map{}}
	k.start(ctx, tp, req)
	time.Sleep(time.Second)
	assert.Contains(t, tp.targets, "https://www.google.com")
}

func TestGetConfig(t *testing.T) {
	cfgFile, err := ioutil.TempFile(t.TempDir(), "config.yml")
	assert.Equal(t, nil, err)

	content := `
  targets:
    - addr: https://www.google.com
      interval: 10s
      labels:
        pop: bur`

	cfgFile.Write([]byte(content))
	cfg, err := getConfig(cfgFile.Name())
	assert.Equal(t, nil, err)
	assert.Len(t, cfg.Targets, 1)
	assert.Equal(t, "https://www.google.com", cfg.Targets[0].Addr)
	assert.Equal(t, "10s", cfg.Targets[0].Interval)
	assert.Equal(t, map[string]string{"pop": "bur"}, cfg.Targets[0].Labels)

	_, err = getConfig("notfound")
	assert.NotNil(t, err)

	cfgFile, err = ioutil.TempFile(t.TempDir(), "config.yml")
	assert.Equal(t, nil, err)
	cfgFile.Write([]byte("wrongyaml"))
	_, err = getConfig(cfgFile.Name())
	assert.NotNil(t, err)
}
func TestIsIPAddr(t *testing.T) {
	assert.True(t, isIPAddr("8.8.8.8"))
	assert.False(t, isIPAddr("www.yahoo.com"))
}
