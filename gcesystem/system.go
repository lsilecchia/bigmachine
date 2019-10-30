package gcesystem

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/grailbio/base/retry"
	"github.com/grailbio/bigmachine"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/http2"
)

const (
	key        = "/home/dazwilkin/.ssh/google_compute_engine"
	port       = 8443
	prefix     = "bigmachine"
	systemName = "gce"
)
const (
	httpTimeout = 30 * time.Second
)

var _ bigmachine.System = (*System)(nil)

var (
	Instance = new(System)
)

func init() {
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		// TODO(dazwilkin) When this System is running locally, the environment variable is required. When this System is running on a GCE Instance, it will be obtained automatically
		// TODO(dazwilkin) Possibly check for the Metadata Service here to help with this decision?
		log.Println("Compute Engine backend uses Application Default Credentials. GOOGLE_APPLICATION_CREDENTIALS environment variable is unset")
	}
	bigmachine.RegisterSystem(systemName, new(System))
}

type System struct {
	Project        string
	Zone           string
	BootstrapImage string
}

func (s *System) Exit(code int) {
	log.Println("[gce:Exit] Entered")
	ctx := context.TODO()
	err := NewClient(ctx)
	if err != nil {
		log.Println("[gce:Exit] unable to delete Compute Engine client")
	}
	// Determine which instances belong to bigmachine using the Tag used when Create'ing
	names, err := List(ctx, s.Project, s.Zone)
	if err != nil {
		log.Println("[gce:Exit] unable to enumerate machines")
	}
	// Delete these instances
	for _, name := range names {
		log.Printf("[gce:Exit] Deleting %s", name)
		Delete(ctx, s.Project, s.Zone, name)
	}
	os.Exit(code)
}
func (s *System) HTTPClient() *http.Client {
	// TODO(dazwilkin) not yet implement
	log.Println("[gce:HTTPClient] Entered")
	transport := &http.Transport{
		// TODO(dazwilkin) Replaced deprecated "Dial" with "DialContext"
		DialContext: (&net.Dialer{
			Timeout: httpTimeout,
		}).DialContext,
	}
	http2.ConfigureTransport(transport)
	return &http.Client{Transport: transport}
}
func (s *System) KeepaliveConfig() (period, timeout, rpcTimeout time.Duration) {
	log.Println("[gce:KeepAliveConfig] Entered")
	period = time.Minute
	timeout = 10 * time.Minute
	rpcTimeout = 2 * time.Minute
	return
}
func (s *System) ListenAndServe(addr string, handler http.Handler) error {
	log.Println("[gce:ListenAndServe] Entered")
	if addr == "" {
		log.Printf("[gce:ListenAndServe] no address provided")
		addr = fmt.Sprintf(":%d", port)
	}
	log.Printf("[gce:ListenAndServe] address: %s", addr)
	// config.ClientAuth = tls.RequireAndVerifyClientCert
	server := &http.Server{
		// TLSConfig: config,
		Addr:    addr,
		Handler: handler,
	}
	http2.ConfigureServer(server, &http2.Server{
		// MaxConcurrentStreams: maxConcurrentStreams,
	})
	//return server.ListenAndServeTLS("", "")
	return server.ListenAndServe()
}
func (s *System) Main() error {
	log.Println("[gce:Main] Entered")
	return http.ListenAndServe(":3333", nil)
}

// MaxProcs returns the number of vCPUs in the instance
// TODO(dazwilkin) Implement MaxProcs so that it returns the actual number of vCPUs on the instance
func (s *System) Maxprocs() int {
	log.Println("[gce:Maxprocs] Entered")
	log.Println("[gce:Maxprocs] Return constant value (1) -- implement to return actual vCPUs")
	return 1
}

// Name returns the name of this system
func (s *System) Name() string {
	return systemName
}
func (s *System) Init(b *bigmachine.B) error {
	log.Println("[gce:Init] Entered")
	// TODO(dazwilkin) Investigate https://godoc.org/github.com/grailbio/base/config per https://github.com/grailbio/bigmachine/issues/1
	// TODO(dazwilkin) Assuming environmental variables (used during development) for the System configuration
	s.Project = os.Getenv("PROJECT")
	s.Zone = os.Getenv("ZONE")
	s.BootstrapImage = fmt.Sprintf("%s:%s", os.Getenv("IMG"), os.Getenv("TAG"))
	return nil
}
func (s *System) Read(ctx context.Context, m *bigmachine.Machine, filename string) (io.Reader, error) {
	log.Println("[gce:Read] Entered")
	return nil, nil
}

// Per Marius this is a graceful shutdown of System that indirectly (!) results in machine's Exit'ing
func (s *System) Shutdown() {
	log.Println("[gce:Shutdown] Entered")

}

// Start attempts to create 'count' GCE instances returns a list of machines and (!) any failures
func (s *System) Start(ctx context.Context, count int) ([]*bigmachine.Machine, error) {
	log.Println("[gce:Start] Entered")
	if count == 0 {
		log.Println("[gce:Start] warning: request to create 0 (zero) instances")
		return []*bigmachine.Machine{}, nil
	}
	if count < 0 {
		return nil, fmt.Errorf("[gce:Start] unable to create <0 instances")
	}

	err := NewClient(ctx)
	if err != nil {
		return nil, err
	}

	type Result struct {
		machine *bigmachine.Machine
		err     error
	}
	var wg sync.WaitGroup

	// Create buffered (non-blocking) channel since we know the number of machines
	// Results will either be success(bigmachine.Machine) or error
	ch := make(chan Result, count)

	// Iterate over Machine creation writing results to the channel
	// Results are Operations or Errors
	for i := 0; i < count; i++ {
		wg.Add(1)
		// TODO(dazwilkin) Convenient (during testing) to name this way; can't create more if existing instances haven't been deleted
		name := fmt.Sprintf("%s-%02d", prefix, i)
		go func(name string) {
			defer wg.Done()
			machine, err := Create(ctx, s.Project, s.Zone, name, s.BootstrapImage)
			ch <- Result{
				machine: machine,
				err:     err,
			}
		}(name)
	}

	log.Println("[gce:Start] await completion of Go routines")
	wg.Wait()
	log.Println("[gce:Start] Go routines have completed")
	close(ch)

	// Proccess the channel of Results
	// If there were errors, there will be fewer than 'count' machines
	var machines []*bigmachine.Machine
	var failures uint
	log.Println("[gce:Start] Iterate over the channel")
	for i := range ch {
		if i.err != nil {
			log.Printf("[gce:Start:go] %s", err)
			failures = failures + 1
		}
		log.Println("[gce:Start] Adding bigmachine")
		machines = append(machines, i.machine)
	}
	log.Println("[gce:Start] Done w/ channel")
	if failures > 0 {
		err = fmt.Errorf("[gcs:Start] %d/%d machines were not created", failures, count)
	}
	log.Println("[gce:Start] Completed")
	return machines, err
}
func (s *System) Tail(ctx context.Context, m *bigmachine.Machine) (io.Reader, error) {
	log.Println("[gce:Tail] Entered")
	u, err := url.Parse(m.Addr)
	if err != nil {
		return nil, err
	}
	// TODO(dazwilkin) unclear whether there's a specific unit that's relevant; EC2 implementation using cloud-init to create `bootmachine`
	// TODO(dazwilkin) these (container logs) would be better using gcloud logging read
	// resource.type="gce_instance"
	// logName="projects/${PROJECT}/logs/cos_containers"
	// resource.labels.instance_id="${INSTANCE_ID}"
	return s.run(ctx, u.Hostname(), "sudo journalctl --output=cat --follow"), nil
}
func (s *System) run(ctx context.Context, addr, command string) io.Reader {
	r, w := io.Pipe()
	go func() {
		var err error
		for retries := 0; ; retries++ {
			err = s.runSSH(addr, w, command)
			if err == nil {
				break
			}
			log.Printf("tail %v: %v", addr, err)
			if strings.HasPrefix(err.Error(), "ssh: unable to authenticate") {
				break
			}
			if _, ok := err.(*ssh.ExitError); ok {
				break
			}
			var sshRetryPolicy = retry.Backoff(time.Second, 10*time.Second, 1.5)
			if err = retry.Wait(ctx, sshRetryPolicy, retries); err != nil {
				break
			}
		}
		w.CloseWithError(err)
	}()
	return r
}
func (s *System) runSSH(addr string, w io.Writer, command string) error {
	conn, err := s.dialSSH(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout = w
	sess.Stderr = w
	return sess.Run(command)
}
func (s *System) dialSSH(addr string) (*ssh.Client, error) {
	// TOOD(dazwilkin) Determine gCloud current user correctly
	log.Println("[system:dialSSH] warning -- defaults to 'dazwilkin' user")
	config := &ssh.ClientConfig{
		User: "dazwilkin",
		Auth: []ssh.AuthMethod{
			s.publicKey(),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
		Timeout: 15 * time.Second,
	}
	return ssh.Dial("tcp", addr+":22", config)
}
func (s *System) publicKey() ssh.AuthMethod {
	buffer, err := ioutil.ReadFile(key)
	if err != nil {
		log.Fatal(err)
	}
	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		log.Fatal(err)
	}
	return ssh.PublicKeys(key)
}