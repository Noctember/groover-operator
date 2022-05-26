package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"github.com/nats-io/nats.go"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ConnectionInfo struct {
	UserId    uint64 `json:"user_id"`
	Endpoint  string `json:"endpoint"`
	GuildId   uint64 `json:"guild_id"`
	Token     string `json:"token"`
	SessionId string `json:"session_id"`
}

type StartMessage struct {
	GuildID string `json:"guild_id"`
	UserID  string `json:"user_id"`
}

type Message struct {
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
}

type Operator struct {
	session   *discordgo.Session
	rdb       *redis.Client
	clientset *kubernetes.Clientset
	nc        *nats.Conn

	sessionId   string
	authifyUrl  string
	authifyAuth string
}

func main() {
	redisAddr := os.Getenv("REDIS_ADDR")
	redisAuth := os.Getenv("REDIS_AUTH")

	if redisAddr == "" {
		panic("REDIS_ADDR not defined")
	}

	natsAddr := os.Getenv("NATS_URL")

	if natsAddr == "" {
		println("NATS_URL not defined")
	}

	authifyUrl := os.Getenv("AUTHIFY_URL")
	authifyAuth := os.Getenv("AUTHIFY_KEY")
	if authifyUrl == "" {
		panic("AUTHIFY_URL not defined")
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisAuth,
	})

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	nc, err := nats.Connect(natsAddr)
	if err != nil {
		println(natsAddr)
		panic(err.Error())
	}

	session, err := discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))
	if err != nil {
		panic(err.Error())
	}
	op := Operator{
		session:   session,
		rdb:       rdb,
		clientset: clientset,
		nc:        nc,

		authifyUrl:  authifyUrl,
		authifyAuth: authifyAuth,
	}

	session.AddHandler(func(s *discordgo.Session, vc *discordgo.Ready) {
		op.sessionId = vc.SessionID
	})

	session.State.TrackMembers = false
	session.State.TrackChannels = false
	session.State.TrackEmojis = false
	session.State.TrackRoles = false
	session.State.TrackPresences = false
	session.State.TrackThreadMembers = false
	session.State.TrackThreads = false

	session.Identify.Intents = discordgo.IntentGuildVoiceStates
	session.AddHandler(op.HandleVoiceUpdates)
	session.Open()

	watchlist := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", "groover",
		fields.Everything())
	_, controller := cache.NewInformer(
		watchlist,
		&v1.Pod{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				if pod.Status.Phase == v1.PodFailed {
					clientset.CoreV1().Pods("groover").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pod := newObj.(*v1.Pod)
				if pod.Status.Phase == v1.PodFailed {
					clientset.CoreV1().Pods("groover").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}
			},
		},
	)
	stop := make(chan struct{})
	go controller.Run(stop)

	nc.Subscribe("ready", op.HandleGrooverReady)
	nc.Subscribe("stop", op.HandleStop)
	nc.Subscribe("login", op.HandleLogin)
	_, err = nc.Subscribe("start", op.HandleStart)

	if err != nil {
		panic(err.Error())
	}
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

func (op *Operator) HandleVoiceUpdates() func(s *discordgo.Session, vc *discordgo.VoiceServerUpdate) {
	return func(s *discordgo.Session, vc *discordgo.VoiceServerUpdate) {
		id := vc.GuildID
		guildId, _ := strconv.ParseInt(vc.GuildID, 10, 64)
		userId, _ := strconv.ParseInt(op.session.State.User.ID, 10, 64)
		op.Publish(id, struct {
			Info ConnectionInfo `json:"info"`
		}{
			ConnectionInfo{
				UserId:    uint64(userId),
				Endpoint:  vc.Endpoint,
				GuildId:   uint64(guildId),
				Token:     vc.Token,
				SessionId: op.sessionId,
			},
		})
	}
}

func (op *Operator) Publish(subject string, msg interface{}) {
	msgBytes, err := json.Marshal(Message{Type: "Join", Value: msg})
	if err != nil {
		fmt.Printf("%e\n", err)
		return
	}
	op.nc.Publish(subject, msgBytes)
}

func (op *Operator) HandleGrooverReady(m *nats.Msg) {
	id := string(m.Data)
	userId := op.rdb.Get(context.Background(), fmt.Sprintf("groover:%s", id))
	voiceState, _ := op.session.State.VoiceState(id, userId.Val())
	op.session.ChannelVoiceJoinManual(id, voiceState.ChannelID, false, true)
}

func (op *Operator) HandleLogin(m *nats.Msg) {
	id := string(m.Data)

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/url?id=%s", op.authifyUrl, id), nil)
	if op.authifyAuth != "" {
		req.Header.Set("Authorization", op.authifyAuth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	url, _ := ioutil.ReadAll(resp.Body)
	m.Respond(url)
}

func (op *Operator) HandleStop(m *nats.Msg) {
	id := string(m.Data)
	msg, _ := json.Marshal(Message{Type: "Stop", Value: struct{}{}})
	op.nc.Publish(id, msg)
	op.rdb.Del(context.Background(), fmt.Sprintf("groover:%s", id))
	m.Respond([]byte("Dipping!"))
}

func (op *Operator) HandleStart(m *nats.Msg) {
	var msg StartMessage
	json.Unmarshal(m.Data, &msg)

	if op.rdb.Exists(context.Background(), fmt.Sprintf("oauth:%s", msg.UserID)).Val() != 1 {
		m.Respond([]byte("You are not logged in, use **/login** to connect your spotify account!"))
		return
	}

	state, err := op.session.State.VoiceState(msg.GuildID, msg.UserID)
	if err == discordgo.ErrNilState || state == nil {
		m.Respond([]byte("You are not in a voice channel!"))
		return
	}
	if op.rdb.Exists(context.Background(), fmt.Sprintf("groover:%s", msg.GuildID)).Val() == 1 {
		m.Respond([]byte("There's already a listening party!"))
		return
	}
	file, err := ioutil.ReadFile("deployment.json")
	if err != nil {
		fmt.Printf("While opening deployment: %v\n", err)
		return
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/token?id=%s", op.authifyUrl, msg.UserID), nil)
	if op.authifyAuth != "" {
		req.Header.Set("Authorization", op.authifyAuth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	token, _ := ioutil.ReadAll(resp.Body)

	fileStr := string(file)
	fileStr = strings.ReplaceAll(fileStr, "$GUILD_ID", msg.GuildID)
	fileStr = strings.ReplaceAll(fileStr, "$USER_ID", msg.UserID)
	fileStr = strings.ReplaceAll(fileStr, "$TOKEN", string(token))
	fileStr = strings.ReplaceAll(fileStr, "$POD_NAME", "groover-"+msg.GuildID)

	var pod v1.Pod
	err = json.Unmarshal([]byte(fileStr), &pod)
	if err != nil {
		fmt.Printf("While unmarshalling pod: %v\n", err)
		return
	}

	_, err = op.clientset.CoreV1().Pods("groover").Create(context.Background(), &pod, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("While creating pod: %v\n", err)
		return
	}
	op.rdb.Set(context.Background(), fmt.Sprintf("groover:%s", msg.GuildID), msg.UserID, 0)
	m.Respond([]byte("Starting up! I will join your voice channel once I'm ready."))
}
