package b2bua

import (
	"fmt"

	"github.com/cloudwebrtc/go-sip-ua/pkg/account"
	"github.com/cloudwebrtc/go-sip-ua/pkg/auth"
	"github.com/cloudwebrtc/go-sip-ua/pkg/registry"
	"github.com/cloudwebrtc/go-sip-ua/pkg/session"
	"github.com/cloudwebrtc/go-sip-ua/pkg/stack"
	"github.com/cloudwebrtc/go-sip-ua/pkg/ua"
	"github.com/cloudwebrtc/go-sip-ua/pkg/util"
	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/transport"
	"github.com/sirupsen/logrus"
)

type B2BCall struct {
	src  *session.Session
	dest *session.Session
}

func (b *B2BCall) ToString() string {
	return b.src.Contact() + " => " + b.dest.Contact()
}

// B2BUA .
type B2BUA struct {
	ua       *ua.UserAgent
	accounts map[string]string
	registry registry.Registry
	domains  []string
	calls    []*B2BCall
	rfc8599  *registry.RFC8599
}

var (
	logger log.Logger
)

func init() {
	logger = util.NewLogrusLogger(logrus.DebugLevel).WithPrefix("B2BUA")
}

//NewB2BUA .
func NewB2BUA(pushCallback registry.PushCallback) *B2BUA {
	b := &B2BUA{
		registry: registry.Registry(&*registry.NewMemoryRegistry()),
		accounts: make(map[string]string),
		rfc8599:  registry.NewRFC8599(pushCallback),
	}

	stack := stack.NewSipStack(&stack.SipStackConfig{
		Extensions: []string{"replaces", "outbound"},
		Dns:        "8.8.8.8",
		ServerAuthManager: stack.ServerAuthManager{
			Authenticator:     auth.NewServerAuthorizer(b.requestCredential, "b2bua", false, logger),
			RequiresChallenge: b.requiresChallenge,
		},
	}, logger)

	stack.OnConnectionError(b.handleConnectionError)

	if err := stack.Listen("udp", "0.0.0.0:5060"); err != nil {
		logger.Panic(err)
	}

	if err := stack.Listen("tcp", "0.0.0.0:5060"); err != nil {
		logger.Panic(err)
	}

	tlsOptions := &transport.TLSConfig{Cert: "certs/cert.pem", Key: "certs/key.pem"}

	if err := stack.ListenTLS("tls", "0.0.0.0:5061", tlsOptions); err != nil {
		logger.Panic(err)
	}

	if err := stack.ListenTLS("wss", "0.0.0.0:5081", tlsOptions); err != nil {
		logger.Panic(err)
	}

	ua := ua.NewUserAgent(&ua.UserAgentConfig{
		UserAgent: "Go B2BUA/1.0.0",
		SipStack:  stack,
	}, logger)

	ua.InviteStateHandler = func(sess *session.Session, req *sip.Request, resp *sip.Response, state session.Status) {
		logger.Infof("InviteStateHandler: state => %v, type => %s", state, sess.Direction())

		switch state {
		// Handle incoming call.
		case session.InviteReceived:
			to, _ := (*req).To()
			from, _ := (*req).From()
			aor := to.Address

			doInvite := func(instance *registry.ContactInstance) {
				displayName := ""
				if from.DisplayName != nil {
					displayName = from.DisplayName.String()
				}
				profile := account.NewProfile(from.Address.User().String(), displayName, nil, 0)
				target := "sip:" + aor.User().String() + "@" + instance.Source + ";transport=" + instance.Transport
				offer := sess.RemoteSdp()
				dest, err := ua.Invite(profile, target, &offer)
				if err != nil {
					logger.Errorf("B-Leg session error: %v", err)
					return
				}
				b.calls = append(b.calls, &B2BCall{src: sess, dest: dest})
			}

			// Try to find online contact records.
			if contacts, found := b.registry.GetContacts(aor); found {
				sess.Provisional(100, "Trying")
				for _, instance := range *contacts {
					doInvite(instance)
				}
				return
			}

			// Pushable: try to find pn-params in contact records.
			// Try to push the UA and wait for it to wake up.
			pusher, ok := b.rfc8599.TryPush(aor, from)
			if ok {
				sess.Provisional(100, "Trying")
				instance, err := pusher.WaitContactOnline()
				if err != nil {
					logger.Errorf("Push failed, error: %v", err)
					sess.Reject(500, fmt.Sprint("Push failed"))
					return
				}
				doInvite(instance)
				return
			}

			// Could not found any records
			sess.Reject(404, fmt.Sprintf("%v Not found", aor))
			break

		// Handle re-INVITE or UPDATE.
		case session.ReInviteReceived:
			logger.Infof("re-INVITE")
			switch sess.Direction() {
			case session.Incoming:
				sess.Accept(200)
			case session.Outgoing:
				//TODO: Need to provide correct answer.
			}
			break

		// Handle 1XX
		case session.EarlyMedia:
			fallthrough
		case session.Provisional:
			call := b.findCall(sess)
			if call != nil && call.dest == sess {
				answer := call.dest.RemoteSdp()
				call.src.ProvideAnswer(answer)
				call.src.Provisional((*resp).StatusCode(), (*resp).Reason())
			}
			break

		// Handle 200OK or ACK
		case session.Confirmed:
			call := b.findCall(sess)
			if call != nil && call.dest == sess {
				answer := call.dest.RemoteSdp()
				call.src.ProvideAnswer(answer)
				call.src.Accept(200)
			}
			break

		// Handle 4XX+
		case session.Failure:
			fallthrough
		case session.Canceled:
			fallthrough
		case session.Terminated:
			call := b.findCall(sess)
			if call != nil {
				if call.src == sess {
					call.dest.End()
				} else if call.dest == sess {
					call.src.End()
				}
			}
			b.removeCall(sess)
			break

		}
	}

	ua.RegisterStateHandler = func(state account.RegisterState) {
		logger.Infof("RegisterStateHandler: state => %v", state)
	}

	stack.OnRequest(sip.REGISTER, b.handleRegister)
	b.ua = ua
	return b
}

func (b *B2BUA) Calls() []*B2BCall {
	return b.calls
}

func (b *B2BUA) findCall(sess *session.Session) *B2BCall {
	for _, call := range b.calls {
		if call.src == sess || call.dest == sess {
			return call
		}
	}
	return nil
}

func (b *B2BUA) removeCall(sess *session.Session) {
	for idx, call := range b.calls {
		if call.src == sess || call.dest == sess {
			b.calls = append(b.calls[:idx], b.calls[idx+1:]...)
			return
		}
	}
}

//Shutdown .
func (b *B2BUA) Shutdown() {
	b.ua.Shutdown()
}

func (b *B2BUA) requiresChallenge(req sip.Request) bool {
	switch req.Method() {
	//case sip.UPDATE:
	case sip.REGISTER:
		return true
	case sip.INVITE:
		return true
	//case sip.RREFER:
	//	return false
	case sip.CANCEL:
		return false
	case sip.OPTIONS:
		return false
	case sip.INFO:
		return false
	case sip.BYE:
		{
			// Allow locally initiated dialogs
			// Return false if call-id in sessions.
			return false
		}
	}
	return false
}

//AddAccount .
func (b *B2BUA) AddAccount(username string, password string) {
	b.accounts[username] = password
}

//GetAccounts .
func (b *B2BUA) GetAccounts() map[string]string {
	return b.accounts
}

//GetRegistry .
func (b *B2BUA) GetRegistry() registry.Registry {
	return b.registry
}

func (b *B2BUA) requestCredential(username string) (string, string, error) {
	if password, found := b.accounts[username]; found {
		logger.Infof("Found user %s", username)
		return password, "", nil
	}
	return "", "", fmt.Errorf("username [%s] not found", username)
}

func (b *B2BUA) handleRegister(request sip.Request, tx sip.ServerTransaction) {
	headers := request.GetHeaders("Expires")
	to, _ := request.To()
	aor := to.Address.Clone()
	var expires sip.Expires = 0
	if len(headers) > 0 {
		expires = *headers[0].(*sip.Expires)
	}

	reason := ""
	if len(headers) > 0 && expires != sip.Expires(0) {
		instance := registry.NewContactInstanceForRequest(request)
		logger.Infof("Registered [%v] expires [%d] source %s", to, expires, request.Source())
		reason = "Registered"
		b.registry.AddAor(aor, instance)
		b.rfc8599.HandleContactInstance(aor, instance)
	} else {
		logger.Infof("Logged out [%v] expires [%d] ", to, expires)
		reason = "UnRegistered"
		instance := registry.NewContactInstanceForRequest(request)
		b.registry.RemoveContact(aor, instance)
		b.rfc8599.HandleContactInstance(aor, instance)
	}

	resp := sip.NewResponseFromRequest(request.MessageID(), request, 200, reason, "")
	sip.CopyHeaders("Expires", request, resp)
	util.BuildContactHeader("Contact", request, resp, &expires)
	sip.CopyHeaders("Content-Length", request, resp)
	tx.Respond(resp)

}

func (b *B2BUA) handleConnectionError(connError *transport.ConnectionError) {
	logger.Debugf("Handle Connection Lost: Source: %v, Dest: %v, Network: %v", connError.Source, connError.Dest, connError.Net)
	b.registry.HandleConnectionError(connError)
}
