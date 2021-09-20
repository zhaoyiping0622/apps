package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"gopkg.in/gomail.v2"
)

type Config struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Imap     struct {
		Server string `json:"server"`
		Port   int    `json:"port"`
	} `json:"imap"`
	Smtp struct {
		Server string `json:"server"`
		Port   int    `json:"port"`
	} `json:"smtp"`
	Timeout       int      `json:"timeout"`
	DataDirectory string   `json:"DataDirectory"`
	Keys          []string `json:"keys"`
}

var config Config

func readConfig() {
	if s, err := os.ReadFile("config.json"); err != nil {
		log.Panicln(err)
	} else {
		if err := json.Unmarshal(s, &config); err != nil {
			log.Panicln(err)
		}
		if err := os.MkdirAll(config.DataDirectory, os.ModePerm); err != nil {
			log.Panicln(err)
		}
		for _, key := range config.Keys {
			if err := os.MkdirAll(path.Join(config.DataDirectory, key), os.ModePerm); err != nil {
				log.Panicln(err)
			}
		}
	}
}

func sendReply(to string, subject string, attachmentNames []string, date time.Time) {
	d := gomail.NewDialer(config.Smtp.Server, config.Smtp.Port, config.Username, config.Password)
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	m := gomail.NewMessage()
	m.SetHeader("From", config.Username)
	m.SetHeader("To", to)
	m.SetHeader("Subject", "回执："+subject)
	body := fmt.Sprintf("发送时间为\"%v\"标题为\"%v\"的邮件已收到，收取到的附件如下：%v\n 此邮件由代码生成，请勿回复，谢谢。", date, subject, attachmentNames)
	m.SetBody("text/plain", body)
	log.Printf("from %v reply to %v, content %v", config.Username, to, body)
	if err := d.DialAndSend(m); err != nil {
		log.Println(err)
	}
}

func connect(config *Config) *client.Client {
	cli, err := client.DialTLS(fmt.Sprintf("%s:%d", config.Imap.Server, config.Imap.Port), &tls.Config{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Panicln(err)
	}
	log.Println("Connected")
	return cli
}

func login(cli *client.Client, config *Config) {
	if err := cli.Login(config.Username, config.Password); err != nil {
		log.Panicln(err)
	}
	log.Println("Logged in")
}

func fileExist(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}

func saveFile(reader io.Reader, filename string) string {
	filename = path.Join(config.DataDirectory, filename)
	if fileExist(filename) {
		for i := 0; ; i++ {
			if !fileExist(fmt.Sprintf("%s-%d", filename, i)) {
				filename = fmt.Sprintf("%s-%d", filename, i)
				break
			}
		}
	}
	f, err := os.Create(filename)
	if err != nil {
		log.Printf("err when create file %s, err: %v\n", filename, err)
	}
	defer f.Close()
	io.Copy(f, reader)
	return filename
}

func processMail(msg *imap.Message, section *imap.BodySectionName) {
	defer func() {
		if err := recover(); err != nil {
			log.Println(err)
			return
		}
	}()
	body := msg.GetBody(section)
	if body == nil {
		panic("server didn't reaturn message body")
	}
	bodyReader, err := mail.CreateReader(body)
	if err != nil {
		panic(err)
	}
	header := bodyReader.Header
	subject, err := header.Subject()
	if err != nil {
		panic(err)
	}
	var dirname string
	for _, key := range config.Keys {
		if strings.HasPrefix(subject, key) {
			dirname = key
			break
		}
	}
	date, err := header.Date()
	if err != nil {
		log.Println(err)
	}
	reply := true
	from, err := header.AddressList("From")
	if err != nil {
		reply = false
	}
	log.Printf("get message from %v date %v subject %v\n", from, date, subject)
	attachmentNames := make([]string, 0)
	for {
		p, err := bodyReader.NextPart()
		if err == io.EOF {
			log.Printf("msg %v end", subject)
			break
		} else if err != nil {
			log.Panic(err)
		}
		switch data := p.Header.(type) {
		case *mail.InlineHeader:
			b, _ := ioutil.ReadAll(p.Body)
			log.Printf("get text %v", string(b))
		case *mail.AttachmentHeader:
			filename, err := data.Filename()
			if err != nil {
				log.Printf("fail to get attachment name, err %v\n", err)
				continue
			}
			log.Printf("get attachment %v\n", filename)
			attachmentNames = append(attachmentNames, filename)
			filename = saveFile(p.Body, path.Join(dirname, filename))
			log.Printf("save to file %v", filename)
		}
	}
	if reply {
		sendReply(from[0].Address, subject, attachmentNames, date)
	}
}

func getMessages(cli *client.Client, seqNums []uint32, items []imap.FetchItem) (chan *imap.Message, *imap.BodySectionName) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(seqNums...)
	section := &imap.BodySectionName{}
	items = append(items, imap.FetchEnvelope, imap.FetchFlags, section.FetchItem())
	messages := make(chan *imap.Message, 10)
	go func(seqset *imap.SeqSet, items []imap.FetchItem, messages chan *imap.Message) {
		cli.Fetch(seqset, items, messages)
	}(seqset, items, messages)
	return messages, section
}

func getValidSeqNumber(cli *client.Client) []uint32 {
	cli.Select("INBOX", true)
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	uids, err := cli.Search(criteria)
	if err != nil {
		log.Println(err)
		return nil
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, section.FetchItem()}
	messages, _ := getMessages(cli, uids, items)
	ret := make([]uint32, 0)
	for msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Envelope == nil {
			log.Println("msg Envelope is nil")
			continue
		}
		valid := false
		for _, key := range config.Keys {
			if strings.HasPrefix(msg.Envelope.Subject, key) {
				valid = true
				break
			}
		}
		if valid {
			ret = append(ret, msg.SeqNum)
		}
	}
	return ret
}

func processReceiveMails(cli *client.Client) {
	all := getValidSeqNumber(cli)
	if len(all) != 0 {
		log.Printf("msg %v to read", all)
		_, err := cli.Select("INBOX", false)
		if err != nil {
			log.Println(err)
			return
		}
		messages, section := getMessages(cli, all, []imap.FetchItem{})
		for msg := range messages {
			processMail(msg, section)
		}
		log.Println("processReceiveMails finish")
	}
}

func main() {

	readConfig()
	log.Printf("%+v", config)
	log.Println("Connecting to server...")
	imap.CharsetReader = charset.Reader

	// Connect to server
	cli := connect(&config)

	login(cli, &config)

	// Don't forget to logout
	defer cli.Logout()

	// List mailboxes
	mailboxes := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- cli.List("", "*", mailboxes)
	}()

	log.Println("Mailboxes:")
	for m := range mailboxes {
		log.Println("* " + m.Name)
	}

	if err := <-done; err != nil {
		log.Fatal(err)
	}

	for {
		processReceiveMails(cli)
		time.Sleep(time.Duration(config.Timeout) * time.Second)
	}
}
