package main

import (
	"io"
	"net"
	"os"
	"os/user"

	"github.com/FranGM/simplelog"
	"github.com/flynn/go-shlex"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type scpOptions struct {
	To           bool
	From         bool
	TargetIsDir  bool
	Recursive    bool
	PreserveMode bool
	fileNames    []string
}

type scpConfig struct {
	User           string
	passwords      map[string]string
	Dir            string
	privateKey     ssh.Signer
	PrivateKeyFile string
	Port           string
	AuthKeys       map[string][]ssh.PublicKey
	AuthKeysFile   string
	OneShot        bool // Serve just one connection, then quit (useful for tests)
}

func newScpConfig() *scpConfig {
	osuser, _ := user.Current()
	userHome, _ := os.UserHomeDir()

	privateKeyFile := userHome + "/.ssh/id_rsa"
	authKeysFile := userHome + "/.ssh/authorized_keys"
	return &scpConfig{
		Port:           "8222",
		User:           osuser.Username,
		Dir:            "/",
		PrivateKeyFile: privateKeyFile,
		AuthKeysFile:   authKeysFile,
	}
}

// Allows us to send to the client the exit status code of the command they asked as to run
func sendExitStatusCode(channel ssh.Channel, status uint8) {
	exitStatusBuffer := make([]byte, 4)
	exitStatusBuffer[3] = status
	_, err := channel.SendRequest("exit-status", false, exitStatusBuffer)
	if err != nil {
		// TODO: Don't we prefer to return the error here?
		simplelog.Error.Printf("Failed to forward exit-status to client: %v", err)
	}
}

func handleSFTP(channel ssh.Channel) {

	server, err := sftp.NewServer(channel)
	if err != nil {
		simplelog.Debug.Printf("Failed to start SFTP server: %v", err)
		return
	}
	defer server.Close()

	if err := server.Serve(); err == nil || err == io.EOF {
		simplelog.Debug.Printf("SFTP server exited cleanly")
		sendExitStatusCode(channel, 0)
	} else {
		simplelog.Debug.Printf("SFTP server exited with error: %v", err)
		sendExitStatusCode(channel, 1)
	}

	channel.Close()
}

// Handle requests received through a channel
func (config scpConfig) handleRequest(channel ssh.Channel, req *ssh.Request) {
	ok := true
	simplelog.Debug.Printf("Payload before splitting is %v", string(req.Payload[4:]))
	s, err := shlex.Split(string(req.Payload[4:]))
	if err != nil {
		// TODO: Shouldn't we do something with this error?
		simplelog.Error.Printf("Error when splitting payload: %v", err)
	}

	// Ignore everything that's not scp
	if s[0] != "scp" {
		ok = false
		req.Reply(ok, []byte("Only scp is supported"))
		channel.Write([]byte("Only scp is supported\n"))
		channel.Close()
		return
	}

	opts := scpOptions{}
	// TODO: Do a sanity check of options (like needing to have either -f or -t defined)
	// TODO: Define what happens if both -t and -f are specified?
	// TODO: If we have more than one filename with -t defined it's an error: "ambiguous target"

	// At the very least we expect either -t or -f
	// UNDOCUMENTED scp OPTIONS:
	//  -t: "TO", our server will be receiving files
	//  -f: "FROM", our server will be sending files
	//  -d: Target is expected to be a directory
	// DOCUMENTED scp OPTIONS:
	//  -r: Recursively copy entire directories (follows symlinks)
	//  -p: Preserve modification mtime, atime and mode of files
	parseOpts := true
	opts.fileNames = make([]string, 0)
	for _, elem := range s[1:] {
		if parseOpts {
			switch elem {
			case "-f":
				opts.From = true
			case "-t":
				opts.To = true
			case "-d":
				opts.TargetIsDir = true
			case "-p":
				opts.PreserveMode = true
			case "-r":
				opts.Recursive = true
			case "-v":
				// Verbose mode, this is more of a local client thing
			case "--":
				// After finding a "--" we stop parsing for flags
				if parseOpts {
					parseOpts = false
				} else {
					opts.fileNames = append(opts.fileNames, elem)
				}
			default:
				opts.fileNames = append(opts.fileNames, elem)
			}
		}
	}

	simplelog.Debug.Printf("Called scp with %v", s[1:])
	simplelog.Debug.Printf("Options: %v", opts)
	simplelog.Debug.Printf("Filenames: %v", opts.fileNames)

	// We're acting as source
	if opts.From {
		err := config.startSCPSource(channel, opts)
		ok := true
		if err != nil {
			ok = false
			req.Reply(ok, []byte(err.Error()))
		} else {
			req.Reply(ok, nil)
		}
	}

	// We're acting as sink
	if opts.To {
		var statusCode uint8
		ok := true
		if len(opts.fileNames) != 1 {
			simplelog.Error.Printf("Error in number of targets (ambiguous target)")
			statusCode = 1
			ok = false
			sendErrorToClient("scp: ambiguous target", channel)
		} else {
			config.startSCPSink(channel, opts)
		}
		sendExitStatusCode(channel, statusCode)
		channel.Close()
		req.Reply(ok, nil)
		return
	}
}

func (config scpConfig) handleNewChannel(newChannel ssh.NewChannel) {
	// There are different channel types, depending on what's done at the application level.
	// scp is done over a "session" channel (as it's just used to execute "scp" on the remote side)
	// We reject any other kind of channel as we only care about scp
	simplelog.Debug.Printf("Channel type is %v", newChannel.ChannelType())
	if newChannel.ChannelType() != "session" {
		simplelog.Debug.Printf("Rejecting channel request for type %v", newChannel.ChannelType)
		newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
		return
	}
	channel, requests, err := newChannel.Accept()
	if err != nil {
		// TODO: Don't panic here, just clean up and log error
		panic("could not accept channel.")
	}

	// Inside our channel there are several kinds of requests.
	// We can have a request to open a shell or to set environment variables
	// Again, we only care about "exec" as we will just want to execute scp over ssh
	for req := range requests {
		// scp does an exec, so that's all we care about
		switch req.Type {
		case "exec":
			go config.handleRequest(channel, req)
		case "shell":
			channel.Write([]byte("Opening a shell is not supported by this server\n"))
			req.Reply(false, nil)
		case "env":
			// Ignore these for now
			// TODO: Is there any kind of env settings we want to honor?
			req.Reply(true, nil)
		case "subsystem":
			// SFTP
			if string(req.Payload[4:]) == "sftp" {
				handleSFTP(channel)
				req.Reply(true, nil)
			} else {
				req.Reply(true, nil)
			}
		default:
			simplelog.Debug.Printf("Req type: %v, req payload: %v", req.Type, string(req.Payload))
			req.Reply(true, nil)
		}
	}
}

// Handle new connections
func (c scpConfig) handleConn(nConn net.Conn, config *ssh.ServerConfig) {
	_, chans, _, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		simplelog.Error.Printf("Error during handshake: %v", err)
		return
	}

	// Handle any new channels
	for newChannel := range chans {
		go c.handleNewChannel(newChannel)
	}
	simplelog.Debug.Printf("Finished handling connection from %q", nConn.RemoteAddr())
}

// Parse and return a ssh public key as found in an authorized keys file
func parsePubKey(pktext string) (ssh.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pktext))
	return pub, err
}

func startServer(config *scpConfig, serverConfig *ssh.ServerConfig) {
	// TODO: Add config option/parameter to exit after the first connection (mostly for testing)
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.Port)
	if err != nil {
		simplelog.Fatal.Printf("Failed to listen for connections: %q", err)
	}
	simplelog.Info.Printf("Listening on port %v. Accepting connections", config.Port)
	for {
		nConn, err := listener.Accept()
		if err != nil {
			simplelog.Fatal.Printf("Failed to accept incoming connection: %q", err)
		}
		simplelog.Info.Printf("Accepted connection from %v", nConn.RemoteAddr())
		// TODO: Instead of this, have a method to shut down the server, maybe receiving from a channel
		// ^ To do that server should probably wait until all goroutines are done before shutting down? (unless there's an option to force a close)
		if config.OneShot {
			config.handleConn(nConn, serverConfig)
			break
		}

		go config.handleConn(nConn, serverConfig)
	}
}

func (c scpConfig) initSSHConfig() *ssh.ServerConfig {
	// An SSH server is represented by a ServerConfig, which holds
	// certificate details and handles authentication of ServerConns.
	// Setting NoClientAuth to true would allow users to connect without needing to authenticate
	// TODO: Allow setting NoClientAuth as an option
	serverConfig := &ssh.ServerConfig{
		PasswordCallback:  c.passwordAuth,
		PublicKeyCallback: c.keyAuth,
	}

	serverConfig.AddHostKey(c.privateKey)

	return serverConfig
}

func main() {
	config := initSettings()
	serverConfig := config.initSSHConfig()
	startServer(config, serverConfig)
}
