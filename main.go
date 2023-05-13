package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"

	// "strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/gorilla/websocket"

	"github.com/robfig/cron"
	uuid "github.com/satori/go.uuid"
)

const PT_VERSION string = "1.2.1h3"
const PT_ENV string = "release"

var nodes = make(map[string]*PTNode)   // 节点
var rooms = make(map[string]*RoomWrap) // 房间
var (
	nodesLock = sync.Mutex{}
	roomsLock = sync.Mutex{}
)
var connLocker sync.Map

// Lock 使数据读写不冲突
var conf = new(PTNode)
var isMaster bool // 节点配置

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func wsWriteJson(conn *websocket.Conn, data interface{}) { // 给指定的ws连接写入数据
	lx, _ := connLocker.LoadOrStore(conn, &sync.Mutex{})
	v := lx.(*sync.Mutex)
	v.Lock()
	defer v.Unlock()
	conn.WriteJSON(data)
}

func pzGetUserInfo(access string) (PZUser, error) {
	to := "https://api.phi.zone/user_detail/"

	client := &http.Client{}

	req, _ := http.NewRequest("GET", to, nil)
	req.Header.Add("Accept-Language", "zh-Hans")
	req.Header.Add("User-Agent", "PhiZoneRegularAccess")
	req.Header.Add("Authorization", "Bearer "+access)

	resp, err := client.Do(req)

	if err != nil {
		result := new(PZUser)
		return *result, err
	}

	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)

	result := new(PZUser)

	err2 := json.Unmarshal(body, result)

	if err2 != nil {
		return *result, err2
	}

	return *result, nil
}

func createPlayer(access string) (Player, error) {
	userInfo, err := pzGetUserInfo(access)

	player := Player{
		Id:          strconv.Itoa(int(userInfo.Id)),
		Name:        userInfo.Username,
		Avatar:      userInfo.Avatar,
		ScoreTotal:  0,
		ScoreAvg:    0,
		PlayRecords: make([]*PlayRecord, 0),
		Exited:      0,
		Online:      true,
		IsOwner:     false,
	}

	if err != nil {
		return player, err
	}

	return player, nil
}

func getFreeServer() string {
	var curRate float64
	var curPriority int64
	curPriority = -999999
	curRate = 999999
	cur := ""
	now := time.Now().Unix()
	nodesLock.Lock()
	defer nodesLock.Unlock()
	for k, v := range nodes {
		if now-v.Upd > 120 && v.Ty != "master" {
			continue
		}
		if v.Current >= v.Max {
			continue
		}
		thisRate := float64(float64(v.Current) / float64(v.Max))
		if thisRate < curRate {
			curRate = thisRate
			curPriority = v.Priority
			cur = k
		} else if thisRate == curRate {
			if v.Priority > curPriority {
				curRate = thisRate
				curPriority = v.Priority
				cur = k
			}
		}
	}
	return cur
}

func rpcRoomStateChange(roomId string, typ string) (string, error) {
	to := conf.Master + "/rpc/master/roomChanged/" + roomId + "/" + typ

	client := &http.Client{}

	conf.Upd = time.Now().Unix()
	conf.Current = int64(len(rooms))

	postData, _ := json.Marshal(conf)

	req, _ := http.NewRequest("POST", to, bytes.NewReader(postData))
	req.Header.Add("Accept-Language", "zh-Hans")
	req.Header.Add("User-Agent", "PhiTogetherRPC")
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req)

	if err != nil || resp == nil {
		return "", err
	}

	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)

	k := string(body)

	return k, nil

}

func rpcAlive() {
	to := conf.Master + "/rpc/master/alive"

	client := &http.Client{}

	conf.Upd = time.Now().Unix()
	conf.Current = int64(len(rooms))

	postData, _ := json.Marshal(conf)

	req, _ := http.NewRequest("POST", to, bytes.NewReader(postData))
	req.Header.Add("Accept-Language", "zh-Hans")
	req.Header.Add("User-Agent", "PhiTogetherRPC")
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req)

	if err != nil || resp == nil {
		return
	}

	// defer resp.Body.Close()
	// body, _ := ioutil.ReadAll(resp.Body)
}

func main() {

	// 读取配置
	rd, err := os.ReadFile("config.json")
	if err != nil {
		fmt.Println("读取配置文件失败！退出...")
		return
	}
	json.Unmarshal(rd, conf)
	conf.Current = 0
	isMaster = conf.Ty == "master"

	ptCron := cron.New()                     //计划任务
	ptCron.AddFunc("0 0/4 * * * ?", func() { //4分钟一次
		now := time.Now().Unix()
		roomsLock.Lock()
		defer roomsLock.Unlock()

		for _, v := range rooms {
			if v.Local && !v.Actual.Data.Closed {
				if now-v.Actual.updated > 300 {
					v.Actual.Close()
				}
			}
		}

		if isMaster {
			nodesLock.Lock()
			defer nodesLock.Unlock()

			for k, v := range nodes {
				if now-v.Upd > 300 && v.Ty != "master" {
					delete(nodes, k)
				}
			}
		}
	})

	r := gin.Default() //GIN主服务器部分

	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"POST", "GET"},
		AllowHeaders: []string{"Content-Type,access-control-allow-origin, access-control-allow-headers"},
	}))

	r.SetTrustedProxies([]string{"127.0.0.1"})

	if isMaster {
		nodes[conf.Addr] = conf

		r.LoadHTMLGlob("templates/*")
		r.StaticFS("/static", http.Dir("./static"))
		r.StaticFS("/.well-known", http.Dir("./.well-known"))

		// if true {                                                              // App Store过审了记得把true改回false
		// 	r.StaticFS("/main", http.Dir("./fakeSite"))
		// }

		r.GET("/", func(ctx *gin.Context) {
			// ua := ctx.GetHeader("User-Agent")
			// if strings.Contains(ua, "PTForAppStore/1.6") { // App Store过审了记得把true改回false
			// 	ctx.HTML(http.StatusOK, "fake.html", gin.H{"ver": PT_VERSION}) // 假主页 HTML (骗审核)
			// 	return
			// }
			ctx.HTML(http.StatusOK, "mainplay.html", gin.H{"ver": PT_VERSION,"env": PT_ENV}) // 主页 HTML
		})

		r.GET("/favicon.ico", func(ctx *gin.Context) {
			ctx.File("./static/src/core/favicon.ico")
		})

		r.GET("/sw.v2.js", func(ctx *gin.Context) {
			ctx.File("./static/sw.v2.js")
		})

		rpcMaster := r.Group("rpc/master") //多服务器 主

		rpcMaster.POST("alive", func(ctx *gin.Context) {
			req := new(PTNode)
			if err := ctx.ShouldBindJSON(&req); err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			if conf.Key != req.Key {
				ctx.JSON(http.StatusForbidden, gin.H{"code": "-1"})
				return
			}

			addr := req.Addr

			nodesLock.Lock()
			defer nodesLock.Unlock()

			if v, ok := nodes[addr]; ok {
				v.Current = req.Current
				v.Upd = time.Now().Unix()
			} else {
				nodes[addr] = req
			}

			ctx.JSON(http.StatusOK, gin.H{"code": 0})
		})

		rpcMaster.POST("roomChanged/:roomId/:type", func(ctx *gin.Context) {
			roomId := ctx.Params.ByName("roomId")
			typ := ctx.Params.ByName("type")

			req := new(PTNode)
			if err := ctx.ShouldBindJSON(&req); err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			if conf.Key != req.Key {
				ctx.JSON(http.StatusForbidden, gin.H{"code": "-1"})
				return
			}

			addr := req.Addr

			nodesLock.Lock()
			defer nodesLock.Unlock()

			if v, ok := nodes[addr]; ok {
				v.Current = req.Current
				v.Upd = time.Now().Unix()
			} else {
				nodes[addr] = req
			}

			roomsLock.Lock()
			defer roomsLock.Unlock()

			if v, ok := rooms[roomId]; ok {
				if typ == "lock" {
					v.Actual.Data.Stage = 1
				} else if typ == "close" {
					rooms[roomId] = nil
					delete(rooms, roomId)
				} else {
					ctx.JSON(http.StatusOK, gin.H{"code": -1})
					return
				}
			} else {
				if typ != "close" {
					rooms[roomId] = &RoomWrap{
						Local:    false,
						Location: req.Addr,
						Actual: RoomActual{
							Data: RoomData{
								Stage: 0,
							},
						},
					}
				}
				if typ == "lock" {
					v.Actual.Data.Stage = 1
				}
			}

			ctx.JSON(http.StatusOK, gin.H{"code": 0})

		})

	} else {
		ptCron.AddFunc("0 0/1 * * * ?", rpcAlive) //1分钟一次
		rpcAlive()
	}

	multi := r.Group("api/multi")

	{
		multi.GET("getLatestVersion", func(ctx *gin.Context) { // 版本号接口
			ctx.JSON(http.StatusOK, gin.H{"ver": PT_VERSION})
		})

		multi.GET("requestRoom/:roomId", func(ctx *gin.Context) {
			clientVer := ctx.Query("v")

			if clientVer != PT_VERSION {
				ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "您必须升级您的 PhiTogether 以进行多人游戏"})
				return
			}

			roomId := ctx.Params.ByName("roomId")
			if roomId == "" {
				ctx.JSON(http.StatusBadRequest, gin.H{"code": -1})
				return
			}
			roomsLock.Lock()
			defer roomsLock.Unlock()
			if room, ok := rooms[roomId]; ok {
				if room.Actual.Data.Stage == 0 {
					var location string
					if room.Local {
						location = conf.Addr
					} else {
						location = room.Location
					}
					ctx.JSON(http.StatusOK, gin.H{"code": 0, "msg": "该房间可以加入", "server_addr": location})
				} else {
					ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "该房间比赛已开始"})
				}
			} else {
				server := getFreeServer()
				if server == "" {
					ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "未找到可用的联机服务器"})
					return
				}
				ctx.JSON(http.StatusOK, gin.H{"code": -2, "msg": "房间不存在，您可以创建房间", "server_addr": server})
			}
		})

		multi.POST("createRoom/:roomId", func(ctx *gin.Context) { // 接口：创建房间
			roomId := ctx.Params.ByName("roomId")
			if roomId == "" {
				ctx.JSON(http.StatusBadRequest, gin.H{"code": -1})
				return
			}
			roomsLock.Lock()
			defer roomsLock.Unlock()
			if r, ok := rooms[roomId]; ok {
				if r.Local {
					if !r.Actual.Data.Closed {
						ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "该房间id已被占用"})
						return
					}
				}

			}

			req := new(CreateRoomRequest)
			if err := ctx.ShouldBindJSON(&req); err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			thisplayer, err := createPlayer(req.Access_token)
			if thisplayer.Id == "0" || err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"msg": "error", "code": -3})
				return
			}

			thisplayer.IsOwner = true

			if !isMaster {
				bdy, err := rpcRoomStateChange(roomId, "create")
				if err != nil {
					ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "请求中心服务器出错"})
					return
				}
				if bdy != "{\"code\":0}" {
					ctx.JSON(http.StatusOK, gin.H{"code": -1, "msg": "该房间id已被占用"})
					return
				}
			} else {
				conf.Current++
			}

			rooms[roomId] = &RoomWrap{
				Local:    true,
				Location: "",
				Actual: RoomActual{
					Data: RoomData{
						Id:           roomId,
						Stage:        0,
						PlayerNumber: 1,
						Owner:        thisplayer.Id,
						PlayRound:    0,
						Closed:       false,
						Players: map[string]*Player{
							thisplayer.Id: &thisplayer,
						},
						Evts:       []MultiEvent{},
						PlayRounds: make([]*PlayRound, 0),
						BlackList:  make(map[string]bool),
					},
					Keys:        make(map[string]string),
					Connections: make(map[string]*websocket.Conn),
					Locker:      &sync.Mutex{},
					updated:     time.Now().Unix(),
				},
			}
			ky := uuid.NewV4().String()
			room := &rooms[roomId].Actual

			room.Keys[ky] = thisplayer.Id
			thisplayer.Key = ky

			room.BroadCastAndSave(MultiEvent{
				EvtType: "create",
				Msg:     fmt.Sprintf("%s 创建了 %s 房间", thisplayer.Name, roomId),
			})

			ctx.JSON(http.StatusOK, gin.H{"selfUser": thisplayer, "selfRoom": room.Data, "wsConn": conf.Addr + "/api/multi/ws?uuid=" + ky + "&room=" + roomId, "code": 0})
		})

		multi.POST("joinRoom/:roomId", func(ctx *gin.Context) { // 接口：加入房间
			roomId := ctx.Params.ByName("roomId")
			if roomId == "" {
				ctx.JSON(http.StatusBadRequest, gin.H{"code": -1})
				return
			}
			roomsLock.Lock()
			defer roomsLock.Unlock()

			req := new(CreateRoomRequest)
			if err := ctx.ShouldBindJSON(&req); err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			thisplayer, err := createPlayer(req.Access_token)
			if thisplayer.Id == "0" || err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"msg": "error", "code": -3})
				return
			}

			if r, ok := rooms[roomId]; ok {
				if !r.Local {
					ctx.JSON(http.StatusBadRequest, gin.H{"msg": "Error", "code": -5})
					return
				}
				if e := r.Actual.Data.BlackList[thisplayer.Id]; e {
					ctx.JSON(http.StatusOK, gin.H{"msg": "您已经被移出该房间，无法加入", "code": -2})
					return
				}
				if _, ok := r.Actual.Data.Players[thisplayer.Id]; ok {
					ctx.JSON(http.StatusOK, gin.H{"msg": "该账号已加入此房间，无法加入", "code": -2})
					return
				}
				if r.Actual.Data.PlayerNumber >= 20 {
					ctx.JSON(http.StatusOK, gin.H{"msg": "该房间已达到最大人数", "code": -4})
					return
				}
			} else {
				ctx.JSON(http.StatusOK, gin.H{"msg": "房间不存在", "code": -1})
				return
			}

			rooms[roomId].Actual.Locker.Lock()
			defer rooms[roomId].Actual.Locker.Unlock()

			ky := uuid.NewV4().String()
			room := &rooms[roomId].Actual
			room.Data.PlayerNumber++
			room.Keys[ky] = thisplayer.Id
			thisplayer.Key = ky
			room.Data.Players[thisplayer.Id] = &thisplayer
			room.updated = time.Now().Unix()

			room.BroadCastAndSave(MultiEvent{
				EvtType:   "join",
				Msg:       fmt.Sprintf("%s 加入了房间", thisplayer.Name),
				ExtraInfo: thisplayer,
			})

			ctx.JSON(http.StatusOK, gin.H{"selfUser": thisplayer, "selfRoom": room.Data, "wsConn": conf.Addr + "/api/multi/ws?uuid=" + ky + "&room=" + roomId, "code": 0})
		})

		multi.GET("ws", func(c *gin.Context) {
			ky := c.Query("uuid")
			roomId := c.Query("room")

			if ky == "" || roomId == "" {
				c.JSON(http.StatusBadRequest, gin.H{"code": -1})
				return
			}

			conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
			if err != nil {
				http.NotFound(c.Writer, c.Request)
				return
			}

			roomsLock.Lock()

			var roomWrap *RoomWrap
			var rok bool

			if roomWrap, rok = rooms[roomId]; !rok || !roomWrap.Local {
				conn.WriteJSON(gin.H{"type": "refused"})
				conn.Close()
				roomsLock.Unlock()
				return
			}
			roomsLock.Unlock()

			room := &roomWrap.Actual
			room.Locker.Lock()

			var userId string
			var okx bool
			if userId, okx = room.Keys[ky]; !okx {
				conn.WriteJSON(gin.H{"type": "refused"})
				conn.Close()
				room.Locker.Unlock()
				return
			}
			player := room.Data.Players[userId]

			var (
				oldconn *websocket.Conn
				ok      bool
			)

			if oldconn, ok = room.Connections[userId]; ok {
				lx, _ := connLocker.LoadOrStore(oldconn, &sync.Mutex{})
				v := lx.(*sync.Mutex)
				v.Lock()
				oldconn.WriteJSON(gin.H{"type": "refused"})
				oldconn.Close()
				connLocker.Delete(oldconn)
				v.Unlock()
				delete(room.Connections, userId)
			}

			room.Connections[userId] = conn // 将连接保存

			if ok || !player.Online {
				room.BroadCastAndSave(MultiEvent{
					EvtType:   "reOnline",
					Msg:       fmt.Sprintf("玩家 %s 已重连", player.Name),
					ExtraInfo: player.Id,
				})
			}
			player.Online = true

			room.Locker.Unlock()
			wsWriteJson(conn, gin.H{"type": "alive"})

			for {
				// 读取消息
				_, message, err := conn.ReadMessage()
				if err != nil {
					room.Locker.Lock()
					connLocker.Delete(conn)
					if tmp, ok := room.Connections[userId]; ok && conn == tmp {
						delete(room.Connections, userId)
						room.PlayerDisconnected(userId)
					}
					room.Locker.Unlock()
					break
				}

				room.Locker.Lock()
				room.updated = time.Now().Unix()
				room.Connections[userId] = conn // 将连接保存

				req := new(MultiWSAction)
				json.Unmarshal(message, req)

				if req.Action == "alive" {
					wsWriteJson(conn, gin.H{"type": "alive"})
					room.Locker.Unlock()
					continue
				} else if req.Action == "JITS" {
					reqInfo := req.Data.(map[string]interface{})
					room.Data.ScoreJIT[userId].Score = int32(reqInfo["score"].(float64))
					room.Data.ScoreJIT[userId].Acc = reqInfo["acc"].(float64)
					if reqInfo["first"].(bool) {
						wsWriteJson(conn, gin.H{"type": "allJITS", "extraInfo": room.Data.ScoreJIT})
					}
					room.BroadCast(MultiEvent{
						EvtType:   "JITS",
						ExtraInfo: room.Data.ScoreJIT[userId],
					})
					room.Locker.Unlock()
					continue
				}

				switch req.Action {

				case "getRoomInfo":
					wsWriteJson(conn, gin.H{"type": "roomInfo", "data": room.Data})

				case "userMsg":
					content, ok := req.Data.(string)

					if ok {
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.BroadCastAndSave(MultiEvent{
							EvtType: "userMsg",
							Msg:     fmt.Sprintf("%s 说：%s", player.Name, content),
						})
					}

				case "transferOwnerShip":
					target, ok := req.Data.(string)

					if ok {
						targetPlayer := room.Data.Players[target]
						room.Data.Owner = target
						player.IsOwner = false
						targetPlayer.IsOwner = true
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.BroadCastAndSave(MultiEvent{
							EvtType:   "ownerChanged",
							Msg:       fmt.Sprintf("%s 成为新的房主", targetPlayer.Name),
							ExtraInfo: target,
						})
					}

				case "lockRoom":
					if player.IsOwner {
						room.Data.Stage = 1
						if !isMaster {
							rpcRoomStateChange(roomId, "lock")
						}
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.BroadCastAndSave(MultiEvent{
							EvtType: "lock",
							Msg:     "房间玩家名单锁定，游戏即将开始",
						})
					}

				case "kickPlayer":
					target, ok := req.Data.(string)

					if ok {
						if (userId == target) || player.IsOwner {
							room.Data.PlayerNumber--
							if target == room.Data.Owner {
								room.Close()
								wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
								return
							} else {
								exitType := 0
								if userId == target {
									exitType = 1
								} else {
									room.Data.BlackList[target] = true
									exitType = 2
								}

								room.BroadCastAndSave(MultiEvent{
									EvtType: "exit",
									Msg:     fmt.Sprintf("%s 退出房间", room.Data.Players[target].Name),
									ExtraInfo: gin.H{
										"id": room.Data.Players[target].Id,
									},
								})

								if room.Data.Stage == 0 { // 准备阶段，直接删除玩家
									targetKy := room.Data.Players[target].Key
									if targetConn, ok := room.Connections[target]; ok {
										lx, _ := connLocker.LoadOrStore(targetConn, &sync.Mutex{})
										targetLock := lx.(*sync.Mutex)
										targetLock.Lock()
										targetConn.Close()
										connLocker.Delete(targetConn)
										delete(room.Connections, target)
										targetLock.Unlock()
									}
									delete(room.Keys, targetKy)
									delete(room.Data.Players, target)
								} else { // 其他阶段，标记玩家为退出
									player := room.Data.Players[target]
									targetKy := player.Key
									if targetConn, ok := room.Connections[target]; ok {
										lx, _ := connLocker.LoadOrStore(targetConn, &sync.Mutex{})
										targetLock := lx.(*sync.Mutex)
										targetLock.Lock()
										targetConn.Close()
										connLocker.Delete(targetConn)
										delete(room.Connections, target)
										targetLock.Unlock()
									}
									delete(room.Keys, targetKy)
									player.Exited = int8(exitType)
								}

								wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})

								room.CheckAutoStageChange()
							}

						}
					}
				case "syncChartInfo":
					if player.IsOwner {
						reqInfo := req.Data.(map[string]interface{})
						ctInfo := SyncChartInfo{
							SongData: PZSongChartData{
								Name: reqInfo["songData"].(map[string]interface{})["name"].(string),
							},
							ChartData: PZSongChartData{
								Level: reqInfo["chartData"].(map[string]interface{})["level"].(string),
							},
							SpeedInfo: MultiSpeedInfo{
								Disp: reqInfo["speedInfo"].(map[string]interface{})["disp"].(string),
							},
						}
						var notifyStr string
						switch reqInfo["chartData"].(map[string]interface{})["difficulty"].(type) {
						case string:
							ctInfo.ChartData.Difficulty = reqInfo["chartData"].(map[string]interface{})["difficulty"].(string)
							notifyStr = fmt.Sprintf("谱面 %s %s Lv.%s %s 已被选定", ctInfo.SongData.Name, ctInfo.ChartData.Level, ctInfo.ChartData.Difficulty, ctInfo.SpeedInfo.Disp)
						case float64:
							ctInfo.ChartData.Difficulty = float32(reqInfo["chartData"].(map[string]interface{})["difficulty"].(float64))
							notifyStr = fmt.Sprintf("谱面 %s %s Lv.%.1f %s 已被选定", ctInfo.SongData.Name, ctInfo.ChartData.Level, ctInfo.ChartData.Difficulty, ctInfo.SpeedInfo.Disp)
						}
						room.Data.PlayRounds = append(room.Data.PlayRounds, &PlayRound{
							Scores:    make(map[string]*PlayRecord),
							Loaded:    make(map[string]bool),
							ChartInfo: req.Data,
							N:         room.Data.PlayRound,
							Finished:  false,
						})
						room.Data.Stage = 5
						room.BroadCastAndSave(MultiEvent{
							EvtType:   "loadChart",
							Msg:       notifyStr,
							ExtraInfo: req.Data,
						})
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
					}
				case "loadChartFinish":
					if v, ok := room.Data.PlayRounds[room.Data.PlayRound].Loaded[userId]; (!ok) && (!v) {
						room.Data.PlayRounds[room.Data.PlayRound].Loaded[userId] = true
						room.BroadCastAndSave(MultiEvent{
							EvtType: "chartLoadFinish",
							Msg:     fmt.Sprintf("玩家 %s 已完成谱面加载，加载人数 %v/%v", room.Data.Players[userId].Name, len(room.Data.PlayRounds[room.Data.PlayRound].Loaded), room.Data.PlayerNumber),
						})
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.CheckAutoStageChange()
					} else {
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
					}
				case "startGamePlay":
					if player.IsOwner {
						room.Data.Stage = 3
						room.Data.ScoreJIT = nil
						room.Data.ScoreJIT = make(map[string]*JITScore)
						for name, p := range room.Data.Players {
							room.Data.ScoreJIT[name] = &JITScore{
								Name:  p.Name,
								Id:    p.Id,
								Score: 0,
								Acc:   0,
							}
						}
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.BroadCastAndSave(MultiEvent{
							EvtType: "gameStart",
							Msg:     "游戏开始",
						})
					}
				case "nextTrack":
					if player.IsOwner {
						room.Data.Stage = 1
						room.Data.PlayRound++
						wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})
						room.BroadCastAndSave(MultiEvent{
							EvtType: "nextTrack",
							Msg:     "下一轮游戏开始",
						})
					}
				case "uploadScoreInfo":
					reqInfo := req.Data.(map[string]interface{})
					scInfo := PlayRecord{
						AccNum:   float32(reqInfo["accNum"].(float64)),
						Bad:      int32(reqInfo["bad"].(float64)),
						Perfect:  int32(reqInfo["perfect"].(float64)),
						ScoreStr: reqInfo["scoreStr"].(string),
						AccStr:   reqInfo["accStr"].(string),
						All:      int32(reqInfo["all"].(float64)),
						Good:     int32(reqInfo["good"].(float64)),
						Great:    int32(reqInfo["great"].(float64)),
						ScoreNum: int32(reqInfo["scoreNum"].(float64)),
						Maxcombo: int32(reqInfo["maxcombo"].(float64)),
						Extra:    reqInfo["extra"].(string),
					}
					currentRound := room.Data.PlayRounds[room.Data.PlayRound]
					currentPlayer := room.Data.Players[userId]

					// 更新房间数据
					scInfo.Name = currentPlayer.Name
					currentRound.Scores[userId] = &scInfo

					// 更新玩家数据
					currentPlayer.PlayRecords = append(currentPlayer.PlayRecords, &scInfo)
					var scoresTotal int32 = 0
					var keysPerfect int32 = 0
					var keysGood int32 = 0
					var keysTotal int32 = 0
					count := 0
					for _, i := range currentPlayer.PlayRecords {
						keysPerfect += i.Perfect
						keysGood += i.Good
						keysTotal += i.All
						scoresTotal += i.ScoreNum
						count++
					}
					currentPlayer.ScoreTotal = scoresTotal
					currentPlayer.ScoreAvg = float64(scoresTotal) / float64(count)
					currentPlayer.AccAvg = float64(keysPerfect)/float64(keysTotal) + float64(keysGood)/float64(keysTotal)*0.65

					room.BroadCastAndSave(MultiEvent{
						EvtType:   "sbScoreUploaded",
						ExtraInfo: currentPlayer,
						Msg:       fmt.Sprintf("玩家 %s 已完成分数上传，上传人数 %v/%v", currentPlayer.Name, len(currentRound.Scores), room.Data.PlayerNumber),
					})

					room.CheckAutoStageChange()

					wsWriteJson(conn, gin.H{"type": "success", "data": req.Action})

				}

				room.Locker.Unlock()

			}
		})
	}

	ptCron.Start()

	r.Run("0.0.0.0:" + conf.Port)
}

type PTNode struct {
	Max      int64  `json:"max"`
	Ty       string `json:"type"`
	Addr     string `json:"addr"`
	Port     string `json:"port"`
	Master   string `json:"master"`
	Upd      int64  `json:"upd"`
	Current  int64  `json:"current"`
	Priority int64  `json:"priority"`
	Key      string `json:"key"`
}

type PZUser struct {
	Id       int32  `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar"`
}

type PlayRecord struct {
	Name     string  `json:"name"`
	AccNum   float32 `json:"accNum"`
	AccStr   string  `json:"accStr"`
	All      int32   `json:"all"`
	Bad      int32   `json:"bad"`
	Good     int32   `json:"good"`
	Great    int32   `json:"great"`
	Perfect  int32   `json:"perfect"`
	ScoreNum int32   `json:"scoreNum"`
	ScoreStr string  `json:"scoreStr"`
	Maxcombo int32   `json:"maxcombo"`
	Extra    string  `json:"extra"`
}

type JITScore struct {
	Name  string  `json:"name"`
	Id    string  `json:"id"`
	Score int32   `json:"score"`
	Acc   float64 `json:"acc"`
}

type Player struct {
	Id          string        `json:"id"`
	Name        string        `json:"name"`
	Avatar      string        `json:"avatar"`
	ScoreTotal  int32         `json:"scoreTotal"`
	ScoreAvg    float64       `json:"scoreAvg"`
	AccAvg      float64       `json:"accAvg"`
	PlayRecords []*PlayRecord `json:"playRecord"`
	Exited      int8          `json:"exited"`
	Online      bool          `json:"online"`
	Key         string        `json:"-"`
	IsOwner     bool          `json:"isOwner"`
}

type MultiEvent struct {
	EvtType   string      `json:"type"`
	Msg       string      `json:"msg"`
	Time      int64       `json:"time"`
	ExtraInfo interface{} `json:"extraInfo,omitempty"`
}

type PlayRound struct {
	Scores    map[string]*PlayRecord `json:"scores"`
	Loaded    map[string]bool        `json:"loaded"`
	ChartInfo interface{}            `json:"chartInfo"`
	N         int16                  `json:"n"`
	Finished  bool                   `json:"finished"`
}

type RoomData struct {
	Id           string               `json:"id"`
	Stage        int8                 `json:"stage"`
	PlayerNumber int16                `json:"playerNumber"`
	Players      map[string]*Player   `json:"players"`
	Owner        string               `json:"owner"`
	Evts         []MultiEvent         `json:"evt"`
	PlayRound    int16                `json:"playRound"`
	PlayRounds   []*PlayRound         `json:"playRounds"`
	ScoreJIT     map[string]*JITScore `json:"-"`
	BlackList    map[string]bool      `json:"blackList"`
	Closed       bool                 `json:"closed"`
}

type RoomActual struct {
	Data        RoomData
	Connections map[string]*websocket.Conn
	Keys        map[string]string
	Locker      *sync.Mutex
	updated     int64
}

type RoomWrap struct {
	Local    bool
	Location string
	Actual   RoomActual
}

type CreateRoomRequest struct {
	Access_token string `json:"access_token"`
}

type MultiWSAction struct {
	Action string      `json:"action"`
	Data   interface{} `json:"data,omitempty"`
}

type PZSongChartData struct {
	Id         int32       `json:"id"`
	Name       string      `json:"name"`
	Level      string      `json:"level"`
	Difficulty interface{} `json:"difficulty"`
}

type MultiSpeedInfo struct {
	Val  int8   `json:"val"`
	Disp string `json:"disp"`
}

type SyncChartInfo struct {
	SongData  PZSongChartData `json:"songData"`
	ChartData PZSongChartData `json:"chartData"`
	SpeedInfo MultiSpeedInfo  `json:"speedInfo"`
}

func (room *RoomActual) PlayerDisconnected(userId string) {
	if room.Data.Closed {
		return
	}
	var player *Player
	var ok bool
	if player, ok = room.Data.Players[userId]; ok {
		player.Online = false
		room.BroadCastAndSave(MultiEvent{
			EvtType:   "offline",
			Msg:       fmt.Sprintf("玩家 %s 断开连接", player.Name),
			ExtraInfo: player.Id,
		})
	}
}

func (room *RoomActual) BroadCast(e MultiEvent) {
	bt, _ := json.Marshal(e)
	for userId, v1 := range room.Connections {
		lx, _ := connLocker.LoadOrStore(v1, &sync.Mutex{})
		v1lock := lx.(*sync.Mutex)
		v1lock.Lock()
		err := v1.WriteMessage(websocket.TextMessage, bt)
		if err != nil {
			connLocker.Delete(v1)
			delete(room.Connections, userId)
			if tmp, ok := room.Connections[userId]; ok && v1 == tmp {
				delete(room.Connections, userId)
				room.PlayerDisconnected(userId)
			}
			continue
		}
		v1lock.Unlock()
	}
}

func (room *RoomActual) BroadCastAndSave(e MultiEvent) {
	timeObj := time.Now()
	e.Time = timeObj.Unix() * 1000
	e.Msg = "[" + timeObj.Format("2006-01-02 15:04:05") + "] " + e.Msg
	room.Data.Evts = append(room.Data.Evts, e)
	room.BroadCast(e)
}

func (room *RoomActual) Close() {
	room.Data.Closed = true
	room.BroadCastAndSave(MultiEvent{
		EvtType:   "close",
		Msg:       "房间关闭",
		ExtraInfo: room.Data,
	})
	for _, v := range room.Connections {
		lx, _ := connLocker.LoadOrStore(v, &sync.Mutex{})
		targetLock := lx.(*sync.Mutex)
		targetLock.Lock()
		v.Close()
		targetLock.Unlock()
	}
	id := room.Data.Id
	rooms[id] = nil
	delete(rooms, id)
	if !isMaster {
		rpcRoomStateChange(id, "close")
	} else {
		conf.Current--
	}
}

func (room *RoomActual) CheckAutoStageChange() {
	if len(room.Data.PlayRounds) > int(room.Data.PlayRound) {
		currentRound := room.Data.PlayRounds[room.Data.PlayRound]

		allUploaded := true

		for k, vx := range room.Data.Players {
			if vx.Exited != 0 {
				continue
			}
			if _, ok := currentRound.Scores[k]; !ok {
				allUploaded = false
				break
			}
		}

		if room.Data.Stage == 3 && allUploaded {
			room.Data.Stage = 4
			room.Data.PlayRounds[room.Data.PlayRound].Finished = true
			room.BroadCastAndSave(MultiEvent{
				EvtType:   "allScoreUploaded",
				Msg:       "所有玩家已经完成分数上传",
				ExtraInfo: currentRound,
			})
		}

		allLoaded := true

		for k, vx := range room.Data.Players {
			if vx.Exited != 0 {
				continue
			}
			if _, ok := currentRound.Loaded[k]; !ok {
				allLoaded = false
				break
			}
		}

		if room.Data.Stage == 5 && allLoaded {
			room.Data.Stage = 2
			room.BroadCastAndSave(MultiEvent{
				EvtType: "allLoadFinish",
				Msg:     "所有玩家已经完成谱面加载，游戏可以开始",
			})
		}
	}

}
