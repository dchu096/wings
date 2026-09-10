package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/router/tokens"
	wsutil "github.com/0x7d8/wings/router/websocket"
	"github.com/0x7d8/wings/server/devtools"
)

const (
	devtoolsPermissionConnect = "websocket.connect"
	devtoolsPermissionConsole = "devtools.console"

	eventDevtoolsStdin  = wsutil.Event("devtools.stdin")
	eventDevtoolsStdout = wsutil.Event("devtools.stdout")
	eventDevtoolsResize = wsutil.Event("devtools.resize")
	eventDevtoolsStatus = wsutil.Event("devtools.status")
	eventDevtoolsError  = wsutil.Event("devtools.error")
)

var devtoolsUpgrader = websocket.Upgrader{
	HandshakeTimeout: time.Second * 5,
	CheckOrigin: func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		if o == config.Get().PanelLocation {
			return true
		}
		for _, origin := range config.Get().AllowedOrigins {
			if origin == "*" || origin == o {
				return true
			}
		}
		return false
	},
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// getServerDevtools returns the DevTools sidecar status.
func getServerDevtools(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.DevTools()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools unavailable"})
		return
	}
	c.JSON(http.StatusOK, mgr.Status(c.Request.Context()))
}

// postServerDevtools enables or disables the sidecar for a server.
func postServerDevtools(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.BindJSON(&body); err != nil {
		return
	}

	s.SetDevToolsEnabled(body.Enabled)

	mgr := s.DevTools()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools unavailable"})
		return
	}
	mgr.SetImageOverride(s.Config().DevToolsImage)
	if err := mgr.SetEnabled(c.Request.Context(), body.Enabled && !s.IsSuspended()); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, mgr.Status(c.Request.Context()))
}

// postServerDevtoolsPower starts/stops/destroys the sidecar.
func postServerDevtoolsPower(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var body struct {
		Action string `json:"action"`
	}
	if err := c.BindJSON(&body); err != nil {
		return
	}

	mgr := s.DevTools()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools unavailable"})
		return
	}

	ctx := c.Request.Context()
	var err error
	switch body.Action {
	case "start":
		err = mgr.Start(ctx)
	case "stop":
		err = mgr.Stop(ctx)
	case "destroy":
		err = mgr.Destroy(ctx)
	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "action must be start, stop, or destroy"})
		return
	}
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, mgr.Status(ctx))
}

// getServerDevtoolsSSHHook creates (or rotates) an ephemeral SSH listener using the
// node-configured port pool (or a random high port when the pool is empty).
func getServerDevtoolsSSHHook(c *gin.Context) {
	startServerDevtoolsSSH(c, devtools.SSHSessionRequest{})
}

// postServerDevtoolsSSH creates or rotates ephemeral SSH credentials.
// Optional JSON body may include port / ports / ssh_ports / port_pool / preferred_ports.
// Empty body uses the node config pool only.
func postServerDevtoolsSSH(c *gin.Context) {
	req, ok := parseDevtoolsSSHPortRequest(c)
	if !ok {
		return
	}
	startServerDevtoolsSSH(c, req)
}

func startServerDevtoolsSSH(c *gin.Context, req devtools.SSHSessionRequest) {
	s := middleware.ExtractServer(c)
	if s.IsSuspended() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "server is suspended"})
		return
	}
	if !config.Get().System.DevTools.Enabled {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools feature disabled on this node"})
		return
	}

	mgr := s.DevTools()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools unavailable"})
		return
	}
	if !s.Config().DevToolsEnabled && !mgr.Enabled() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "devtools not enabled for this server"})
		return
	}

	creds, err := mgr.StartSSHSession(c.Request.Context(), req)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"contract":             devtools.ContractVersion,
		"mode":                 "ephemeral_ssh",
		"implemented":          true,
		"port":                 creds.Port,
		"username":             creds.Username,
		"password":             creds.Password,
		"expires_at":           creds.ExpiresAt,
		"idle_timeout_seconds": creds.IdleTimeoutSeconds,
		"session_ttl_seconds":  creds.SessionTTLSeconds,
		"note":                 "SSH to the node FQDN/IP on the returned port. Listener is temporary; not port 22.",
	})
}

// parseDevtoolsSSHPortRequest reads optional Atlas port hints from the POST body.
// Returns ok=false when the handler already aborted (invalid JSON).
func parseDevtoolsSSHPortRequest(c *gin.Context) (devtools.SSHSessionRequest, bool) {
	var out devtools.SSHSessionRequest
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return out, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, true
	}

	var body struct {
		Port           int   `json:"port"`
		Ports          []int `json:"ports"`
		SSHPorts       []int `json:"ssh_ports"`
		PortPool       []int `json:"port_pool"`
		PreferredPorts []int `json:"preferred_ports"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return out, false
	}

	out.PreferredPort = body.Port
	out.Ports = config.FirstNonEmptySSHPortPool(body.Ports, body.SSHPorts, body.PortPool, body.PreferredPorts)
	return out, true
}

// deleteServerDevtoolsSSH stops the ephemeral SSH listener without disabling DevTools.
func deleteServerDevtoolsSSH(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.DevTools()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools unavailable"})
		return
	}
	mgr.StopSSHSession()
	c.JSON(http.StatusOK, mgr.Status(c.Request.Context()))
}

// getServerDevtoolsWebsocket upgrades to a PTY websocket for the developer terminal.
func getServerDevtoolsWebsocket(c *gin.Context) {
	s := middleware.ExtractServer(c)

	if s.IsSuspended() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "server is suspended"})
		return
	}
	if !config.Get().System.DevTools.Enabled {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "devtools feature disabled on this node"})
		return
	}

	c.Header("Content-Security-Policy", "default-src 'self'")
	c.Header("X-Frame-Options", "DENY")

	conn, err := devtoolsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	var (
		session    *devtools.Session
		jwtPayload *tokens.WebsocketPayload
		writeMu    sync.Mutex
	)

	writeJSON := func(msg wsutil.Message) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(msg)
	}

	sendErr := func(message string) {
		writeJSON(wsutil.Message{Event: eventDevtoolsError, Args: []string{message}})
	}

	go func() {
		select {
		case <-ctx.Done():
		case <-s.Context().Done():
			cancel()
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg wsutil.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Event {
		case wsutil.AuthenticationEvent:
			if len(msg.Args) < 1 {
				sendErr("missing jwt")
				continue
			}
			var payload tokens.WebsocketPayload
			if err := tokens.ParseToken([]byte(msg.Args[0]), &payload); err != nil {
				sendErr("jwt verification failed")
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "jwt verification failed"))
				return
			}
			if payload.Denylisted() || !payload.HasPermission(devtoolsPermissionConnect) {
				sendErr("unauthorized")
				return
			}
			if payload.ServerUUID != s.ID() {
				sendErr("server uuid mismatch")
				return
			}
			if !payload.HasPermission(devtoolsPermissionConsole) {
				sendErr("missing permission: " + string(devtoolsPermissionConsole))
				return
			}
			jwtPayload = &payload
			writeJSON(wsutil.Message{Event: wsutil.AuthenticationSuccessEvent})

			mgr := s.DevTools()
			if mgr == nil {
				sendErr("devtools unavailable")
				return
			}
			if !s.Config().DevToolsEnabled && !mgr.Enabled() {
				sendErr("devtools not enabled for this server")
				return
			}

			sess, err := mgr.AttachPTY(ctx)
			if err != nil {
				sendErr(err.Error())
				writeJSON(wsutil.Message{Event: eventDevtoolsStatus, Args: []string{mgr.Status(ctx).State}})
				continue
			}
			session = sess
			writeJSON(wsutil.Message{Event: eventDevtoolsStatus, Args: []string{"running"}})

			go func(sess *devtools.Session) {
				defer sess.Close()
				buf := make([]byte, 8192)
				for {
					n, rerr := sess.Stdout().Read(buf)
					if n > 0 {
						writeJSON(wsutil.Message{Event: eventDevtoolsStdout, Args: []string{string(buf[:n])}})
					}
					if rerr != nil {
						if rerr != io.EOF {
							log.WithField("error", rerr).Debug("devtools pty stdout closed")
						}
						cancel()
						return
					}
					select {
					case <-ctx.Done():
						return
					default:
					}
				}
			}(session)

		case eventDevtoolsStdin:
			if jwtPayload == nil || session == nil {
				continue
			}
			if len(msg.Args) < 1 {
				continue
			}
			if _, err := session.Stdin().Write([]byte(msg.Args[0])); err != nil {
				sendErr("stdin write failed")
			}

		case eventDevtoolsResize:
			if jwtPayload == nil || session == nil {
				continue
			}
			if len(msg.Args) < 2 {
				continue
			}
			if err := session.ResizeFromStrings(ctx, msg.Args[0], msg.Args[1]); err != nil {
				sendErr(err.Error())
			}
		}
	}

	if session != nil {
		_ = session.Close()
	}
}
