package core

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/teamnsrg/tmail/message"
)

const (
	// CR is a Carriage Return
	CR = 13
	// LF is a Line Feed
	LF = 10
	//ZEROBYTE ="\\0"
)

// SMTPServerSession retpresents a SMTP session (server)
type SMTPServerSession struct {
	uuid    string
	Conn    net.Conn
	connTLS *tls.Conn
	//logger           *logrus.Logger
	timer            *time.Timer // for timeout
	timeout          time.Duration
	tls              bool
	tlsVersion       string
	RelayGranted     bool
	user             *User
	seenHelo         bool
	seenMail         bool
	lastClientCmd    []byte
	helo             string
	Envelope         message.Envelope
	LastRcptTo       string
	exitasap         chan int
	rcptCount        int
	BadRcptToCount   int
	vrfyCount        int
	remoteAddr       string
	SMTPResponseCode uint32
	dataBytes        uint32
	startAt          time.Time
	exiting          bool
	CurrentRawMail   []byte
}

// NewSMTPServerSession returns a new SMTP session
func NewSMTPServerSession(conn net.Conn, isTLS bool) (sss *SMTPServerSession, err error) {
	sss = new(SMTPServerSession)
	sss.uuid, err = NewUUID()
	if err != nil {
		return
	}
	sss.startAt = time.Now()

	sss.Conn = conn
	if isTLS {
		sss.connTLS = conn.(*tls.Conn)
		sss.tls = true
	}

	sss.remoteAddr = conn.RemoteAddr().String()
	//sss.logger = Log

	sss.RelayGranted = false

	sss.rcptCount = 0
	sss.BadRcptToCount = 0
	sss.vrfyCount = 0

	sss.lastClientCmd = []byte{}

	sss.seenHelo = false
	sss.seenMail = false

	// timeout
	sss.exitasap = make(chan int, 1)
	sss.timeout = time.Duration(Cfg.GetSmtpdServerTimeout()) * time.Second
	sss.timer = time.AfterFunc(sss.timeout, sss.raiseTimeout)
	return
}

// GetLastClientCmd returns lastClientCmd (not splited)
func (s *SMTPServerSession) GetLastClientCmd() []byte {
	return bytes.TrimSuffix(s.lastClientCmd, []byte{13})
}

// GetEnvelope returns pointer to current envelope
// mainly used for plugin
func (s *SMTPServerSession) GetEnvelope() *message.Envelope {
	return &s.Envelope
}

// timeout
func (s *SMTPServerSession) raiseTimeout() {
	s.Log("client timeout")
	s.Out("420 Client timeout")
	s.SMTPResponseCode = 420
	s.ExitAsap()
}

// recoverOnPanic handles panic
func (s *SMTPServerSession) recoverOnPanic() {
	if err := recover(); err != nil {
		s.LogError(fmt.Sprintf("PANIC: %s - Stack: %s", err.(error).Error(), debug.Stack()))
		s.Out("421 sorry I have an emergency")
		s.ExitAsap()
	}
}

// ExitAsap exist session as soon as possible
func (s *SMTPServerSession) ExitAsap() {
	if s.exiting {
		time.Sleep(time.Duration(1) * time.Millisecond)
		return
	}
	s.exiting = true
	if !s.timer.Stop() {
		go func() { <-s.timer.C }()
	}
	// Plugins
	execSMTPdPlugins("exitasap", s)
	s.exitasap <- 1

}

// resetTimeout reset timeout
func (s *SMTPServerSession) resetTimeout() {
	if !s.timer.Stop() {
		go func() { <-s.timer.C }()
	}
	s.timer.Reset(s.timeout)
}

// Reset session
func (s *SMTPServerSession) Reset() {
	s.Envelope.MailFrom = ""
	s.seenMail = false
	s.Envelope.RcptTo = []string{}
	s.rcptCount = 0
	s.resetTimeout()
}

// Out : to client
func (s *SMTPServerSession) Out(msg string) {
	s.Conn.Write([]byte(msg + "\r\n"))
	s.LogDebug(">", msg)
	s.resetTimeout()
}

// Log helper for INFO log
func (s *SMTPServerSession) Log(msg ...string) {
	Logger.Info("smtpd ", s.uuid, "-", s.Conn.RemoteAddr().String(), "-", strings.Join(msg, " "))
}

// LogError is a log helper for ERROR logs
func (s *SMTPServerSession) LogError(msg ...string) {
	Logger.Error("smtpd ", s.uuid, "-", s.Conn.RemoteAddr().String(), "-", strings.Join(msg, " "))
}

// LogDebug is a log helper for DEBUG logs
func (s *SMTPServerSession) LogDebug(msg ...string) {
	if !Cfg.GetDebugEnabled() {
		return
	}
	Logger.Debug("smtpd -", s.uuid, "-", s.Conn.RemoteAddr().String(), "-", strings.Join(msg, " "))
}

// LF withour CR
func (s *SMTPServerSession) strayNewline() {
	s.Log("LF not preceded by CR")
	s.Out("451 You send me LF not preceded by a CR, your SMTP client is broken.")
}

// purgeConn Purge connexion buffer
func (s *SMTPServerSession) purgeConn() (err error) {
	ch := make([]byte, 1)
	for {
		_, err = s.Conn.Read(ch)
		if err != nil {
			return
		}
		/*if ch[0] == 10 {
			break
		}*/
	}
}

// add pause (ex if client seems to be illegitime)
func (s *SMTPServerSession) pause(seconds int) {
	time.Sleep(time.Duration(seconds) * time.Second)
}

// smtpGreeting Greeting
func (s *SMTPServerSession) smtpGreeting() {
	defer s.recoverOnPanic()
	// Todo AS: verifier si il y a des data dans le buffer
	// Todo desactiver server signature en option
	// dans le cas ou l'on refuse la transaction on doit répondre par un 554 et attendre le quit
	time.Sleep(100 * time.Nanosecond)
	if SmtpSessionsCount > Cfg.GetSmtpdConcurrencyIncoming() {
		s.Log(fmt.Sprintf("GREETING - max connections reached %d/%d", SmtpSessionsCount, Cfg.GetSmtpdConcurrencyIncoming()))
		s.Out(fmt.Sprintf("421 sorry, the maximum number of connections has been reached, try again later %s", s.uuid))
		s.SMTPResponseCode = 421
		s.ExitAsap()
		return
	}
	s.Log(fmt.Sprintf("starting new transaction %d/%d", SmtpSessionsCount, Cfg.GetSmtpdConcurrencyIncoming()))

	// Plugins
	if execSMTPdPlugins("connect", s) {
		return
	}

	o := "220 " + Cfg.GetMe() + " ESMTP"
	if !Cfg.GetHideServerSignature() {
		o += " - tmail " + Version
	}
	o += " - " + s.uuid
	s.Out(o)
	if s.tls {
		s.Log("secured via " + tlsGetVersion(s.connTLS.ConnectionState().Version) + " " + tlsGetCipherSuite(s.connTLS.ConnectionState().CipherSuite))
	}
}

// EHLO HELO
// helo do the common EHLO/HELO tasks
func (s *SMTPServerSession) heloBase(msg []string) (cont bool) {
	defer s.recoverOnPanic()
	if s.seenHelo {
		s.Log("EHLO|HELO already received")
		s.pause(1)
		s.Out("503 bad sequence, ehlo already recieved")
		return false
	}

	// Plugins
	if execSMTPdPlugins("helo", s) {
		return false
	}

	s.helo = ""
	if len(msg) > 1 {
		if Cfg.getRFCHeloNeedsFqnOrAddress() {
			// if it's not an address check for fqn
			if net.ParseIP(msg[1]) == nil {
				ok, err := isFQN(msg[1])
				if err != nil {
					s.Log("fail to do lookup on helo host. " + err.Error())
					s.Out("404 unable to resolve " + msg[1] + ". Need fqdn or address in helo command")
					s.SMTPResponseCode = 404
					return false
				}
				if !ok {
					s.Log("helo command rejected, need fully-qualified hostname or address" + msg[1] + " given")
					s.Out("504 helo command rejected, need fully-qualified hostname or address #5.5.2")
					s.SMTPResponseCode = 504
					return false
				}
			}
		}
		s.helo = strings.Join(msg[1:], " ")
	} else if Cfg.getRFCHeloNeedsFqnOrAddress() {
		s.Log("helo command rejected, need fully-qualified hostname. None given")
		s.Out("504 helo command rejected, need fully-qualified hostname or address #5.5.2")
		s.SMTPResponseCode = 504
		return false
	}
	s.seenHelo = true
	return true
}

// HELO
func (s *SMTPServerSession) smtpHelo(msg []string) {
	defer s.recoverOnPanic()
	if s.heloBase(msg) {
		s.Out(fmt.Sprintf("250 %s", Cfg.GetMe()))
	}
}

// EHLO
func (s *SMTPServerSession) smtpEhlo(msg []string) {
	defer s.recoverOnPanic()
	if s.heloBase(msg) {
		s.Out(fmt.Sprintf("250-%s", Cfg.GetMe()))
		// Extensions
		// Size
		s.Out(fmt.Sprintf("250-SIZE %d", Cfg.GetSmtpdMaxDataBytes()))
		s.Out("250-X-PEPPER")
		// STARTTLS
		if !s.tls {
			s.Out("250-STARTTLS")
		}
		// Auth
		s.Out("250 AUTH PLAIN")
	}
}

// MAIL FROM
func (s *SMTPServerSession) smtpMailFrom(msg []string) {
	defer s.recoverOnPanic()
	extension := []string{}

	// Reset
	s.Reset()

	// cmd EHLO ?
	if Cfg.getRFCHeloMandatory() && !s.seenHelo {
		s.pause(2)
		s.Out("503 5.5.2 Send hello first")
		s.SMTPResponseCode = 503
		return
	}
	msgLen := len(msg)
	// mail from ?
	if msgLen == 1 || !strings.HasPrefix(strings.ToLower(msg[1]), "from:") || msgLen > 4 {
		s.Log("MAIL - Bad syntax: %s" + strings.Join(msg, " "))
		s.pause(2)
		s.Out("501 5.5.4 Syntax: MAIL FROM:<address> [SIZE]")
		s.SMTPResponseCode = 501
		return
	}

	// Plugin - hook "mailpre"
	execSMTPdPlugins("mailpre", s)

	// mail from:<user> EXT || mail from: <user> EXT
	if len(msg[1]) > 5 { // mail from:<user> EXT
		t := strings.Split(msg[1], ":")
		s.Envelope.MailFrom = t[1]
		if msgLen > 2 {
			extension = append(extension, msg[2:]...)
		}
	} else if msgLen > 2 { // mail from: user EXT
		s.Envelope.MailFrom = msg[2]
		if msgLen > 3 {
			extension = append(extension, msg[3:]...)
		}
	} else { // null sender
		s.Envelope.MailFrom = ""
	}

	// Extensions size
	if len(extension) != 0 {
		// Only SIZE is supported (and announced)
		if len(extension) > 1 {
			s.Log("MAIL - Bad syntax: " + strings.Join(msg, " "))
			s.pause(2)
			s.Out("501 5.5.4 Syntax: MAIL FROM:<address> [SIZE]")
			s.SMTPResponseCode = 501
			return
		}
		// SIZE
		extValue := strings.Split(extension[0], "=")
		if len(extValue) != 2 {
			s.Log(fmt.Sprintf("MAIL FROM - Bad syntax : %s ", strings.Join(msg, " ")))
			s.pause(2)
			s.Out("501 5.5.4 Syntax: MAIL FROM:<address> [SIZE]")
			s.SMTPResponseCode = 501
			return
		}
		if strings.ToLower(extValue[0]) != "size" {
			s.Log(fmt.Sprintf("MAIL FROM - Unsuported extension : %s ", extValue[0]))
			s.pause(2)
			s.Out("501 5.5.4 Invalid arguments")
			s.SMTPResponseCode = 501
			return
		}
		if Cfg.GetSmtpdMaxDataBytes() != 0 {
			size, err := strconv.ParseInt(extValue[1], 10, 64)
			if err != nil {
				s.Log(fmt.Sprintf("MAIL FROM - bad value for size extension SIZE=%v", extValue[1]))
				s.pause(2)
				s.Out("501 5.5.4 Invalid arguments")
				s.SMTPResponseCode = 501
				return
			}
			if int(size) > Cfg.GetSmtpdMaxDataBytes() {
				s.Log(fmt.Sprintf("MAIL FROM - message exceeds fixed maximum message size %d/%d", size, Cfg.GetSmtpdMaxDataBytes()))
				s.Out("552 message exceeds fixed maximum message size")
				s.SMTPResponseCode = 552
				s.pause(1)
				return
			}
		}
	}

	// remove <>
	s.Envelope.MailFrom = RemoveBrackets(s.Envelope.MailFrom)

	// mail from is valid ?
	reversePathlen := len(s.Envelope.MailFrom)
	if reversePathlen > 0 { // 0 -> null reverse path (bounce)
		if reversePathlen > 256 { // RFC 5321 4.3.5.1.3
			s.Log("MAIL - reverse path is too long: " + s.Envelope.MailFrom)
			s.Out("550 reverse path must be lower than 255 char (RFC 5321 4.5.1.3.1)")
			s.SMTPResponseCode = 550
			s.pause(2)
			return
		}
		localDomain := strings.Split(s.Envelope.MailFrom, "@")
		if len(localDomain) == 1 {
			s.Log("MAIL - invalid address " + localDomain[0])
			s.pause(2)
			s.Out("501 5.1.7 Invalid address")
			s.SMTPResponseCode = 501
			return
			/*
				localDomain = append(localDomain, Cfg.GetMe())
				s.Envelope.MailFrom = localDomain[0] + "@" + localDomain[1]
			*/
		}
		if Cfg.getRFCMailFromLocalpartSize() && len(localDomain[0]) > 64 {
			s.Log("MAIL - local part is too long: " + s.Envelope.MailFrom)
			s.Out("550 local part of reverse path MUST be lower than 65 char (RFC 5321 4.5.3.1.1)")
			s.SMTPResponseCode = 550
			s.pause(2)
			return
		}
		if len(localDomain[1]) > 255 {
			s.Log("MAIL - domain part is too long: " + s.Envelope.MailFrom)
			s.Out("550 domain part of reverse path MUST be lower than 255 char (RFC 5321 4.5.3.1.2)")
			s.SMTPResponseCode = 550
			s.pause(2)
			return
		}
		// domain part should be FQDN
		ok, err := isFQN(localDomain[1])
		if err != nil {
			s.LogError("MAIL - fail to do lookup on domain part. " + err.Error())
			s.Out("451 unable to resolve " + localDomain[1] + " due to timeout or srv failure")
			s.SMTPResponseCode = 451
			return
		}
		if !ok {
			s.Log("MAIL - need fully-qualified hostname. " + localDomain[1] + " given")
			s.Out("550 5.5.2 need fully-qualified hostname for domain part")
			s.SMTPResponseCode = 550
			return
		}
	}
	// Plugin - hook "mailpost"
	execSMTPdPlugins("mailpost", s)
	s.seenMail = true
	s.Log("MAIL FROM " + s.Envelope.MailFrom)
	s.Out("250 ok")
	s.SMTPResponseCode = 250
}

// RCPT TO
func (s *SMTPServerSession) smtpRcptTo(msg []string) {
	defer s.recoverOnPanic()
	var err error
	s.LastRcptTo = ""
	s.rcptCount++
	//s.LogDebug(fmt.Sprintf("RCPT TO %d/%d", s.rcptCount, Cfg.GetSmtpdMaxRcptTo()))
	if Cfg.GetSmtpdMaxRcptTo() != 0 && s.rcptCount > Cfg.GetSmtpdMaxRcptTo() {
		s.Log(fmt.Sprintf("max RCPT TO command reached (%d)", Cfg.GetSmtpdMaxRcptTo()))
		s.Out("451 4.5.3 max RCPT To commands reached for this sessions")
		s.SMTPResponseCode = 451
		return
	}
	// add pause if rcpt to > 10
	if s.rcptCount > 10 {
		s.pause(1)
	}
	if !s.seenMail {
		s.Log("RCPT before MAIL")
		s.pause(2)
		s.Out("503 5.5.1 bad sequence")
		s.SMTPResponseCode = 503
		return
	}

	if len(msg) == 1 || !strings.HasPrefix(strings.ToLower(msg[1]), "to:") {
		s.Log(fmt.Sprintf("RCPT TO - Bad syntax : %s ", strings.Join(msg, " ")))
		s.pause(2)
		s.Out("501 5.5.4 syntax: RCPT TO:<address>")
		s.SMTPResponseCode = 501
		return
	}

	// rcpt to: user
	if len(msg[1]) > 3 {
		t := strings.Split(msg[1], ":")
		s.LastRcptTo = strings.Join(t[1:], ":")
	} else if len(msg) > 2 {
		s.LastRcptTo = msg[2]
	}

	if len(s.LastRcptTo) == 0 {
		s.Log("RCPT - Bad syntax : %s " + strings.Join(msg, " "))
		s.pause(2)
		s.Out("501 5.5.4 syntax: RCPT TO:<address>")
		s.SMTPResponseCode = 501
		return
	}
	s.LastRcptTo = RemoveBrackets(s.LastRcptTo)

	// We MUST recognize source route syntax but SHOULD strip off source routing
	// RFC 5321 4.1.1.3
	t := strings.SplitAfter(s.LastRcptTo, ":")
	s.LastRcptTo = t[len(t)-1]

	// if no domain part and local part is postmaster FRC 5321 2.3.5
	if strings.ToLower(s.LastRcptTo) == "postmaster" {
		s.LastRcptTo += "@" + Cfg.GetMe()
	}
	// Check validity
	_, err = mail.ParseAddress(s.LastRcptTo)
	if err != nil {
		s.Log(fmt.Sprintf("RCPT - bad email format : %s - %s ", strings.Join(msg, " "), err))
		s.pause(2)
		s.Out("501 5.5.4 Bad email format")
		s.SMTPResponseCode = 501
		return
	}

	// rcpt accepted ?
	localDom := strings.Split(s.LastRcptTo, "@")
	if len(localDom) != 2 {
		s.Log(fmt.Sprintf("RCPT - Bad email format : %s ", strings.Join(msg, " ")))
		s.pause(2)
		s.Out("501 5.5.4 Bad email format")
		s.SMTPResponseCode = 501
		return
	}

	// make domain part insensitive
	s.LastRcptTo = localDom[0] + "@" + strings.ToLower(localDom[1])

	// Relay granted for this recipient ?
	s.RelayGranted = false

	// Plugins
	if execSMTPdPlugins("rcptto", s) {
		return
	}

	// check DB for rcpthost
	if !s.RelayGranted {
		rcpthost, err := RcpthostGet(localDom[1])
		if err != nil && err != gorm.ErrRecordNotFound {
			s.LogError("RCPT - relay access failed while queriyng for rcpthost. " + err.Error())
			s.Out("455 4.3.0 oops, problem with relay access")
			s.SMTPResponseCode = 455
			return
		}
		if err == nil {
			// rcpthost exists relay granted
			s.RelayGranted = true
			// if local check "mailbox" (destination)
			if rcpthost.IsLocal {
				s.LogDebug(rcpthost.Hostname + " is local")
				// check destination
				exists, err := IsValidLocalRcpt(strings.ToLower(s.LastRcptTo))
				if err != nil {
					s.LogError("RCPT - relay access failed while checking validity of local rpctto. " + err.Error())
					s.Out("455 4.3.0 oops, problem with relay access")
					s.SMTPResponseCode = 455
					return
				}
				if !exists {
					s.Log("RCPT - no mailbox here by that name: " + s.LastRcptTo)
					s.Out("550 5.5.1 Sorry, no mailbox here by that name")
					s.SMTPResponseCode = 550
					s.BadRcptToCount++
					if Cfg.GetSmtpdMaxBadRcptTo() != 0 && s.BadRcptToCount > Cfg.GetSmtpdMaxBadRcptTo() {
						s.Log("RCPT - too many bad rcpt to, connection droped")
						s.ExitAsap()
					}
					return
				}
			}
		}
	}
	// User authentified & access granted ?
	if !s.RelayGranted && s.user != nil {
		s.RelayGranted = s.user.AuthRelay
	}

	// Remote IP authorised ?
	if !s.RelayGranted {
		s.RelayGranted, err = IpCanRelay(s.Conn.RemoteAddr())
		if err != nil {
			s.LogError("RCPT - relay access failed while checking if IP is allowed to relay. " + err.Error())
			s.Out("455 4.3.0 oops, problem with relay access")
			s.SMTPResponseCode = 455
			return
		}
	}

	// Relay denied
	if !s.RelayGranted {
		s.Log("Relay access denied - from " + s.Envelope.MailFrom + " to " + s.LastRcptTo)
		s.Out("554 5.7.1 Relay access denied")
		s.SMTPResponseCode = 554
		s.pause(2)
		return
	}

	// Check if there is already this recipient
	if !IsStringInSlice(s.LastRcptTo, s.Envelope.RcptTo) {
		s.Envelope.RcptTo = append(s.Envelope.RcptTo, s.LastRcptTo)
		s.Log("RCPT - + " + s.LastRcptTo)
	}
	s.Out("250 ok")
	s.SMTPResponseCode = 250
}

// SMTPVrfy VRFY SMTP command
func (s *SMTPServerSession) smtpVrfy(msg []string) {
	defer s.recoverOnPanic()
	rcptto := ""
	s.vrfyCount++
	s.LogDebug(fmt.Sprintf("VRFY -  %d/%d", s.vrfyCount, Cfg.GetSmtpdMaxVrfy()))
	if Cfg.GetSmtpdMaxVrfy() != 0 && s.vrfyCount > Cfg.GetSmtpdMaxVrfy() {
		s.Log(fmt.Sprintf(" VRFY - max command reached (%d)", Cfg.GetSmtpdMaxVrfy()))
		s.Out("551 5.5.3 too many VRFY commands for this sessions")
		s.SMTPResponseCode = 551
		return
	}
	// add pause if rcpt to > 10
	if s.vrfyCount > 10 {
		s.pause(1)
	} else if s.vrfyCount > 20 {
		s.pause(2)
	}

	if len(msg) != 2 {
		s.Log("VRFY - Bad syntax : %s " + strings.Join(msg, " "))
		s.pause(2)
		s.Out("551 5.5.4 syntax: VRFY <address>")
		s.SMTPResponseCode = 551
		return
	}

	// vrfy: user
	rcptto = msg[1]
	if len(rcptto) == 0 {
		s.Log("VRFY - Bad syntax : %s " + strings.Join(msg, " "))
		s.pause(2)
		s.Out("551 5.5.4 syntax: VRFY <address>")
		s.SMTPResponseCode = 551
		return
	}

	rcptto = RemoveBrackets(rcptto)

	// if no domain part and local part is postmaster FRC 5321 2.3.5
	if strings.ToLower(rcptto) == "postmaster" {
		rcptto += "@" + Cfg.GetMe()
	}
	// Check validity
	_, err := mail.ParseAddress(rcptto)
	if err != nil {
		s.Log(fmt.Sprintf("VRFY - bad email format : %s - %s ", strings.Join(msg, " "), err))
		s.pause(2)
		s.Out("551 5.5.4 Bad email format")
		s.SMTPResponseCode = 551
		return
	}

	// rcpt accepted ?
	localDom := strings.Split(rcptto, "@")
	if len(localDom) != 2 {
		s.Log("VRFY - Bad email format : " + rcptto)
		s.pause(2)
		s.Out("551 5.5.4 Bad email format")
		s.SMTPResponseCode = 551
		return
	}
	// make domain part insensitive
	rcptto = localDom[0] + "@" + strings.ToLower(localDom[1])
	// check rcpthost

	rcpthost, err := RcpthostGet(localDom[1])
	if err != nil && err != gorm.ErrRecordNotFound {
		s.LogError("VRFY - relay access failed while queriyng for rcpthost. " + err.Error())
		s.Out("455 4.3.0 oops, internal failure")
		s.SMTPResponseCode = 455
		return
	}
	if err == nil {
		// if local check "mailbox" (destination)
		if rcpthost.IsLocal {
			s.LogDebug("VRFY - " + rcpthost.Hostname + " is local")
			// check destination
			exists, err := IsValidLocalRcpt(strings.ToLower(rcptto))
			if err != nil {
				s.LogError("VRFY - relay access failed while checking validity of local rpctto. " + err.Error())
				s.Out("455 4.3.0 oops, internal failure")
				s.SMTPResponseCode = 455
				return
			}
			if !exists {
				s.Log("VRFY - no mailbox here by that name: " + rcptto)
				s.Out("551 5.5.1 <" + rcptto + "> no mailbox here by that name")
				s.SMTPResponseCode = 551
				return
			}
			s.Out("250 <" + rcptto + ">")
			s.SMTPResponseCode = 250
			// relay
		} else {
			s.Out("252 <" + rcptto + ">")
			s.SMTPResponseCode = 252
		}
	} else {
		s.Log("VRFY - no mailbox here by that name: " + rcptto)
		s.Out("551 5.5.1 <" + rcptto + "> no mailbox here by that name")
		s.SMTPResponseCode = 551
		return
	}
}

// SMTPExpn EXPN SMTP command
func (s *SMTPServerSession) smtpExpn(msg []string) {
	s.Out("252")
	s.SMTPResponseCode = 252
	return
}

// DATA
// plutot que de stocker en RAM on pourrait envoyer directement les danat
// dans un fichier ne queue
// Si il y a une erreur on supprime le fichier
// Voir un truc comme DATA -> temp file -> mv queue file
func (s *SMTPServerSession) smtpData(msg []string) {
	defer s.recoverOnPanic()
	if !s.seenMail || len(s.Envelope.RcptTo) == 0 {
		s.Log("DATA - out of sequence")
		s.pause(2)
		s.Out("503 5.5.1 command out of sequence")
		s.SMTPResponseCode = 503
		return
	}

	if len(msg) > 1 {
		s.Log("DATA - invalid syntax: " + strings.Join(msg, " "))
		s.pause(2)
		s.Out("501 5.5.4 invalid syntax")
		s.SMTPResponseCode = 551
		return
	}
	s.Out("354 End data with <CR><LF>.<CR><LF>")
	s.SMTPResponseCode = 354

	// Get RAW mail
	//var s.CurrentRawMail []byte
	s.CurrentRawMail = []byte{}
	ch := make([]byte, 1)
	//state := 0
	pos := 0        // position in current line
	hops := 0       // nb of relay
	s.dataBytes = 0 // nb of bytes (size of message)
	flagInHeader := true
	flagLineMightMatchReceived := true
	flagLineMightMatchDelivered := true
	flagLineMightMatchCRLF := true
	state := 1

	doLoop := true

	for {
		if !doLoop {
			break
		}
		s.resetTimeout()
		_, err := s.Conn.Read(ch)
		if err != nil {
			// we will tryc to send an error message to client, but there is a LOT of
			// chance that is gone
			s.LogError("DATA - unable to read byte from conn. " + err.Error())
			s.Out("454 something wrong append will reading data from you")
			s.SMTPResponseCode = 454
			s.ExitAsap()
			return
		}
		if flagInHeader {
			// Check hops
			if pos < 9 {
				if ch[0] != byte("delivered"[pos]) && ch[0] != byte("DELIVERED"[pos]) {
					flagLineMightMatchDelivered = false
				}
				if flagLineMightMatchDelivered && pos == 8 {
					hops++
				}

				if pos < 8 {
					if ch[0] != byte("received"[pos]) && ch[0] != byte("RECEIVED"[pos]) {
						flagLineMightMatchReceived = false
					}
				}
				if flagLineMightMatchReceived && pos == 7 {
					hops++
				}

				if pos < 2 && ch[0] != "\r\n"[pos] {
					flagLineMightMatchCRLF = false
				}

				if (flagLineMightMatchCRLF) && pos == 1 {
					flagInHeader = false
				}
			}
			pos++
			if ch[0] == LF {
				pos = 0
				flagLineMightMatchCRLF = true
				flagLineMightMatchDelivered = true
				flagLineMightMatchReceived = true
			}
		}

		switch state {
		case 0:
			if ch[0] == LF {
				s.strayNewline()
				return
			}
			if ch[0] == CR {
				state = 4
				s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
				s.dataBytes++
				continue
			}
		// \r\n
		case 1:
			if ch[0] == LF {
				s.strayNewline()
				return
			}
			// "."
			if ch[0] == 46 {
				state = 2
				continue
			}
			// "\r"
			if ch[0] == CR {
				state = 4
				s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
				s.dataBytes++
				continue
			}
			state = 0

		// "\r\n +."
		case 2:
			if ch[0] == LF {
				s.strayNewline()
				return
			}
			if ch[0] == CR {
				state = 3
				s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
				s.dataBytes++
				continue
			}
			state = 0

		//\r\n +.\r
		case 3:
			if ch[0] == LF {
				doLoop = false
				s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
				s.dataBytes++
				continue
			}

			if ch[0] == CR {
				state = 4
				s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
				s.dataBytes++
				continue
			}
			state = 0

		// /* + \r */
		case 4:
			if ch[0] == LF {
				state = 1
				break
			}
			if ch[0] != CR {
				s.CurrentRawMail = append(s.CurrentRawMail, 10)
				state = 0
			}
		}
		s.CurrentRawMail = append(s.CurrentRawMail, ch[0])
		s.dataBytes++

		// Max hops reached ?
		if hops > Cfg.GetSmtpdMaxHops() {
			s.Log(fmt.Sprintf("MAIL - Message is looping. Hops : %d", hops))
			s.Out("554 5.4.6 too many hops, this message is looping")
			s.SMTPResponseCode = 554
			s.purgeConn()
			s.Reset()
			return
		}

		// Max databytes reached ?
		if s.dataBytes > uint32(Cfg.GetSmtpdMaxDataBytes()) {
			s.Log(fmt.Sprintf("MAIL - Message size (%d) exceeds maxDataBytes (%d).", s.dataBytes, Cfg.GetSmtpdMaxDataBytes()))
			s.Out("552 5.3.4 sorry, that message size exceeds my databytes limit")
			s.SMTPResponseCode = 552
			s.purgeConn()
			s.Reset()
			return
		}
	}

	// scan
	// clamav
	if Cfg.GetSmtpdClamavEnabled() {
		found, virusName, err := NewClamav().ScanStream(bytes.NewReader(s.CurrentRawMail))
		Logger.Debug("clamav scan result", found, virusName, err)
		if err != nil {
			s.LogError("MAIL - clamav: " + err.Error())
			s.Out("454 4.3.0 scanner failure")
			s.SMTPResponseCode = 454
			//s.purgeConn()
			s.Reset()
			return
		}
		if found {
			s.Out("554 5.7.1 message infected by " + virusName)
			s.SMTPResponseCode = 554
			s.Log("MAIL - infected by " + virusName)
			//s.purgeConn()
			s.Reset()
			return
		}
	}

	// Message-ID
	HeaderMessageID := message.RawGetMessageId(&s.CurrentRawMail)
	if len(HeaderMessageID) == 0 {
		atDomain := Cfg.GetMe()
		if strings.Count(s.Envelope.MailFrom, "@") != 0 {
			atDomain = strings.ToLower(strings.Split(s.Envelope.MailFrom, "@")[1])
		}
		HeaderMessageID = []byte(fmt.Sprintf("%d.%s@%s", time.Now().Unix(), s.uuid, atDomain))
		s.CurrentRawMail = append([]byte(fmt.Sprintf("Message-ID: <%s>\r\n", HeaderMessageID)), s.CurrentRawMail...)

	}
	s.Log("message-id:", string(HeaderMessageID))

	// Add recieved header
	remoteIP := strings.Split(s.Conn.RemoteAddr().String(), ":")[0]
	remoteHost := "no reverse"
	remoteHosts, err := net.LookupAddr(remoteIP)
	if err == nil {
		remoteHost = remoteHosts[0]
	}
	localIP := strings.Split(s.Conn.LocalAddr().String(), ":")[0]
	localHost := "no reverse"
	localHosts, err := net.LookupAddr(localIP)
	if err == nil {
		localHost = localHosts[0]
	}
	recieved := "Received: from "

	// helo
	if len(s.helo) != 0 {
		recieved += fmt.Sprintf("%s ", s.helo)
	}

	recieved += fmt.Sprintf("(%s [%s])", remoteHost, remoteIP)

	// Authentified
	if s.user != nil {
		recieved += fmt.Sprintf(" (authenticated as %s)", s.user.Login)
	}

	// local
	recieved += fmt.Sprintf(" by %s (%s)", localIP, localHost)

	// Proto
	if s.tls {
		recieved += " with ESMTPS " + tlsGetVersion(s.connTLS.ConnectionState().Version) + " " + tlsGetCipherSuite(s.connTLS.ConnectionState().CipherSuite) + "; "
	} else {
		recieved += " whith SMTP; "
	}

	// tmail
	recieved += "tmail " + Version
	recieved += "; " + s.uuid
	// timestamp
	recieved += "; " + time.Now().Format(Time822)
	h := []byte(recieved)
	message.FoldHeader(&h)
	h = append(h, []byte{13, 10}...)
	s.CurrentRawMail = append(h, s.CurrentRawMail...)
	recieved = ""

	s.CurrentRawMail = append([]byte("X-Env-From: "+s.Envelope.MailFrom+"\r\n"), s.CurrentRawMail...)

	// Plugins
	if execSMTPdPlugins("data", s) {
		return
	}

	// put message in queue
	authUser := ""
	if s.user != nil {
		authUser = s.user.Login
	}

	// Plugins
	execSMTPdPlugins("beforequeue", s)
	id, err := QueueAddMessage(&s.CurrentRawMail, s.Envelope, authUser)
	if err != nil {
		s.LogError("MAIL - unable to put message in queue -", err.Error())
		s.Out("451 temporary queue error")
		s.SMTPResponseCode = 451
		s.Reset()
		return
	}
	s.Log("message queued as", id)
	s.Out(fmt.Sprintf("250 2.0.0 Ok: queued %s", id))
	s.SMTPResponseCode = 250
	s.Reset()
	return
}

// QUIT
func (s *SMTPServerSession) smtpQuit() {
	// Plugins
	execSMTPdPlugins("quit", s)

	s.Out(fmt.Sprintf("221 2.0.0 Bye"))
	s.SMTPResponseCode = 221
	s.ExitAsap()
}

// Starttls
func (s *SMTPServerSession) smtpStartTLS() {
	if s.tls {
		s.Out("454 - transaction is already over SSL/TLS")
		s.SMTPResponseCode = 454
		return
	}
	cert, err := tls.LoadX509KeyPair(path.Join(GetBasePath(), "ssl/server.crt"), path.Join(GetBasePath(), "ssl/server.key"))
	if err != nil {
		msg := "TLS failed unable to load server keys: " + err.Error()
		s.LogError(msg)
		s.Out("454 " + msg)
		s.SMTPResponseCode = 454
		return
	}

	tlsConfig := tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}
	tlsConfig.Rand = rand.Reader

	s.Out("220 Ready to start TLS nego")
	s.SMTPResponseCode = 220

	//var tlsConn *tls.Conn
	//tlsConn = tls.Server(client.socket, TLSconfig)
	s.connTLS = tls.Server(s.Conn, &tlsConfig)
	// run a handshake
	// errors.New("tls: unsupported SSLv2 handshake received")
	err = s.connTLS.Handshake()
	if err != nil {
		msg := "454 - TLS handshake failed: " + err.Error()
		s.SMTPResponseCode = 454
		if err.Error() == "tls: unsupported SSLv2 handshake received" {
			s.Log(msg)
		} else {
			s.LogError(msg)
		}
		s.Out(msg)
		return
	}
	s.Log("connection upgraded to " + tlsGetVersion(s.connTLS.ConnectionState().Version) + " " + tlsGetCipherSuite(s.connTLS.ConnectionState().CipherSuite))
	//s.Conn = net.Conn(tlsConn)
	s.Conn = s.connTLS
	s.tls = true
	s.seenHelo = false
}

// SMTP AUTH
// Return boolean closeCon
// Pour le moment in va juste implémenter PLAIN
func (s *SMTPServerSession) smtpAuth(rawMsg string) {
	defer s.recoverOnPanic()
	// TODO si pas TLS
	//var authType, user, passwd string
	//TODO si pas plain

	//
	splitted := strings.Split(rawMsg, " ")
	var encoded string
	if len(splitted) == 3 {
		encoded = splitted[2]
	} else if len(splitted) == 2 {
		// refactor: readline function
		var line []byte
		ch := make([]byte, 1)
		// return a
		s.Out("334 ")
		s.SMTPResponseCode = 334
		// get encoded by reading next line
		for {
			s.resetTimeout()
			_, err := s.Conn.Read(ch)
			if err != nil {
				s.Out("501 malformed auth input (#5.5.4)")
				s.SMTPResponseCode = 501
				s.Log("error reading auth err:" + err.Error())
				s.ExitAsap()
				return
			}
			if ch[0] == 10 {
				s.timer.Stop()
				s.LogDebug("< " + string(line))
				break
			}
			line = append(line, ch[0])
		}

	} else {
		s.Out("501 malformed auth input (#5.5.4)")
		s.SMTPResponseCode = 501
		s.Log("malformed auth input: " + rawMsg)
		s.ExitAsap()
		return
	}

	// decode  "authorize-id\0userid\0passwd\0"
	authData, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		s.Out("501 malformed auth input (#5.5.4)")
		s.SMTPResponseCode = 501
		s.Log("malformed auth input: " + rawMsg + " err:" + err.Error())
		s.ExitAsap()
		return
	}

	// split
	t := make([][]byte, 3)
	i := 0
	for _, b := range authData {
		if b == 0 {
			i++
			continue
		}
		t[i] = append(t[i], b)
	}
	//authId := string(t[0])
	authLogin := string(t[1])
	authPasswd := string(t[2])

	s.user, err = UserGet(authLogin, authPasswd)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			s.Out("535 authentication failed - No such user (#5.7.1)")
			s.SMTPResponseCode = 535
			s.Log("auth failed: " + rawMsg + " err:" + err.Error())
			s.ExitAsap()
			return
		}
		if err.Error() == "crypto/bcrypt: hashedPassword is not the hash of the given password" {
			s.Out("535 authentication failed (#5.7.1)")
			s.SMTPResponseCode = 535
			s.Log("auth failed: " + rawMsg + " err:" + err.Error())
			s.ExitAsap()
			return
		}
		s.Out("454 oops, problem with auth (#4.3.0)")
		s.SMTPResponseCode = 454
		s.Log("ERROR auth " + rawMsg + " err:" + err.Error())
		s.ExitAsap()
		return
	}
	s.Log("auth succeed for user " + s.user.Login)
	s.Out("235 ok, go ahead (#2.0.0)")
	s.SMTPResponseCode = 235
}

// RSET SMTP ahandler
func (s *SMTPServerSession) rset() {
	s.Reset()
	s.Out("250 2.0.0 ok")
	s.SMTPResponseCode = 250
}

// NOOP SMTP handler
func (s *SMTPServerSession) noop() {
	s.Out("250 2.0.0 ok")
	s.SMTPResponseCode = 250
	s.resetTimeout()
}

// Handle SMTP session
func (s *SMTPServerSession) handle() {
	defer s.recoverOnPanic()

	// Init some var
	//var msg []byte

	buffer := make([]byte, 1)

	// welcome (
	s.smtpGreeting()

	go func() {
		defer s.recoverOnPanic()
		for {
			_, err := s.Conn.Read(buffer)
			if err != nil {
				if err.Error() == "EOF" {
					s.LogDebug(s.Conn.RemoteAddr().String(), "- Client send EOF")
				} else if strings.Contains(err.Error(), "connection reset by peer") {
					s.Log(err.Error())
				} else if !strings.Contains(err.Error(), "use of closed network connection") {
					s.LogError("unable to read data from client - ", err.Error())
				}
				s.ExitAsap()
				break
			}

			if buffer[0] == 0x00 {
				continue
			}

			if buffer[0] == 10 {
				s.timer.Stop()
				var rmsg string
				strMsg := strings.TrimSpace(string(s.lastClientCmd))
				s.LogDebug("<", strMsg)
				splittedMsg := []string{}
				for _, m := range strings.Split(strMsg, " ") {
					m = strings.TrimSpace(m)
					if m != "" {
						splittedMsg = append(splittedMsg, m)
					}
				}

				// get command, first word
				// TODO Use textproto / scanner
				if len(splittedMsg) != 0 {
					verb := strings.ToLower(splittedMsg[0])
					switch verb {
					case "helo":
						s.smtpHelo(splittedMsg)
					case "ehlo":
						//s.smtpEhlo(splittedMsg)
						s.smtpEhlo(splittedMsg)
					case "mail":
						s.smtpMailFrom(splittedMsg)
					case "vrfy":
						s.smtpVrfy(splittedMsg)
					case "expn":
						s.smtpExpn(splittedMsg)
					case "rcpt":
						s.smtpRcptTo(splittedMsg)
					case "data":
						s.smtpData(splittedMsg)
					case "starttls":
						s.smtpStartTLS()
					case "auth":
						s.smtpAuth(strMsg)
					case "rset":
						s.rset()
					case "noop":
						s.noop()
					case "quit":
						s.smtpQuit()
					default:
						rmsg = "502 5.5.1 unimplemented"
						s.Log("unimplemented command from client:", strMsg)
						s.Out(rmsg)
						s.SMTPResponseCode = 502
					}
				}
				//s.resetTimeout()
				s.lastClientCmd = []byte{}
			} else {
				s.lastClientCmd = append(s.lastClientCmd, buffer[0])
			}
		}
	}()
	<-s.exitasap
	s.Conn.Close()
	s.Log("EOT")
	s.exiting = false
	return
}
