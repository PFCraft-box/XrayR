package controller

import (
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/common/legocmd"
	"github.com/XrayR-project/XrayR/common/serverstatus"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
)

type Controller struct {
	server                  *core.Instance
	config                  *Config
	clientInfo              api.ClientInfo
	apiClient               api.API
	nodeInfo                *api.NodeInfo
	Tag                     string
	userList                *[]api.UserInfo
	nodeStatus              *api.NodeStatus
	onlineUsers             *[]api.OnlineUser
	userTraffic             *[]api.UserTraffic
	nodeInfoMonitorPeriodic *task.Periodic
	userReportPeriodic      *task.Periodic
}

// New return a Controller service with default parameters.
func New(server *core.Instance, api api.API, config *Config) *Controller {
	controller := &Controller{
		server:    server,
		config:    config,
		apiClient: api,
	}
	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start() error {
	c.clientInfo = c.apiClient.Describe()
	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return err
	}
	// Add new tag
	err = c.addNewTag(newNodeInfo)
	if err != nil {
		log.Panic(err)
		return err
	}
	// Update user
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		return err
	}
	c.nodeInfo = newNodeInfo
	c.Tag = fmt.Sprintf("%s_%d", c.nodeInfo.NodeType, c.nodeInfo.Port)
	err = c.addNewUser(userInfo, newNodeInfo)
	if err != nil {
		return err
	}

	c.userList = userInfo
	// Add Limiter
	if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, userInfo); err != nil {
		log.Print(err)
	}
	// Add Rule Manager
	if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
		log.Printf("Get rule list filed: %s", err)
	} else if len(*ruleList) > 0 {
		if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			log.Print(err)
		}
	}
	c.nodeInfoMonitorPeriodic = &task.Periodic{
		Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
		Execute:  c.nodeInfoMonitor,
	}
	c.userReportPeriodic = &task.Periodic{
		Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
		Execute:  c.userInfoMonitor,
	}
	log.Print("Start monitor node status")
	c.nodeInfoMonitorPeriodic.Start()
	log.Print("Start report node status")
	c.userReportPeriodic.Start()
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	if c.nodeInfoMonitorPeriodic != nil {
		err := c.nodeInfoMonitorPeriodic.Close()
		if err != nil {
			log.Panicf("node info periodic close failed: %s", err)
		}
	}

	if c.nodeInfoMonitorPeriodic != nil {
		err := c.userReportPeriodic.Close()
		if err != nil {
			log.Panicf("user report periodic close failed: %s", err)
		}
	}
	return nil
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		log.Print(err)
		return nil
	}
	
	// Update User
	newUserInfo, err := c.apiClient.GetUserList()
	if err != nil {
		log.Print(err)
		return nil
	}

	var nodeInfoChanged bool = false
	// If nodeInfo changed
	if !reflect.DeepEqual(c.nodeInfo, newNodeInfo) {
		// Remove old tag
		oldtag := c.Tag
		err := c.removeOldTag(oldtag)
		if err != nil {
			log.Print(err)
			return nil
		}
		// Add new tag
		err = c.addNewTag(newNodeInfo)
		if err != nil {
			log.Print(err)
			return nil
		}
		nodeInfoChanged = true
		c.nodeInfo = newNodeInfo
		c.Tag = fmt.Sprintf("%s_%d", newNodeInfo.NodeType, newNodeInfo.Port)
		// Remove Old limiter
		if err = c.DeleteInboundLimiter(oldtag); err != nil {
			log.Print(err)
			return nil
		}
	}

	// Check Rule
	if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
		log.Printf("Get rule list filed: %s", err)
	} else if len(*ruleList) > 0 {
		if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			log.Print(err)
		}
	}

	// Check Cert
	if c.nodeInfo.EnableTLS && (c.config.CertConfig.CertMode == "dns" || c.config.CertConfig.CertMode == "http") {
		lego, err := legocmd.New()
		if err != nil {
			log.Print(err)
		}
		// Xray-core supports the OcspStapling certification hot renew
		_, _, err = lego.RenewCert(c.config.CertConfig.CertDomain, c.config.CertConfig.Email, c.config.CertConfig.CertMode, c.config.CertConfig.Provider, c.config.CertConfig.DNSEnv)
		if err != nil {
			log.Print(err)
		}
	}

	if nodeInfoChanged {
		err = c.addNewUser(newUserInfo, newNodeInfo)
		if err != nil {
			log.Print(err)
			return nil
		}
		// Add Limiter
		if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, newUserInfo); err != nil {
			log.Print(err)
			return nil
		}
	} else {
		deleted, added := compareUserList(c.userList, newUserInfo)
		if len(deleted) > 0 {
			deletedEmail := make([]string, len(deleted))
			for i, u := range deleted {
				deletedEmail[i] = fmt.Sprintf("%s|%s|%d", c.Tag, u.Email, u.UID)
			}
			err := c.removeUsers(deletedEmail, c.Tag)
			if err != nil {
				log.Print(err)
			}
		}
		if len(added) > 0 {
			err = c.addNewUser(&added, c.nodeInfo)
			if err != nil {
				log.Print(err)
			}
			// Update Limiter
			if err := c.UpdateInboundLimiter(c.Tag, &added); err != nil {
				log.Print(err)
			}
		}
		log.Printf("%d user deleted, %d user added", len(deleted), len(added))
	}
	c.userList = newUserInfo
	return nil
}

func (c *Controller) removeOldTag(oldtag string) (err error) {
	err = c.removeInbound(oldtag)
	if err != nil {
		return err
	}
	err = c.removeOutbound(oldtag)
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) addNewTag(newNodeInfo *api.NodeInfo) (err error) {
	inboundConfig, err := InboundBuilder(c.config.ListenIP, newNodeInfo, c.config.CertConfig)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	outBoundConfig, err := OutboundBuilder(newNodeInfo, c.config.EnableDNS)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	return nil
}

func (c *Controller) addNewUser(userInfo *[]api.UserInfo, nodeInfo *api.NodeInfo) (err error) {
	users := make([]*protocol.User, 0)
	if nodeInfo.NodeType == "V2ray" {
		if nodeInfo.EnableVless {
			users = buildVlessUser(c.Tag, userInfo)
		} else {
			users = buildVmessUser(c.Tag, userInfo, nodeInfo.AlterID)
		}
	} else if nodeInfo.NodeType == "Trojan" {
		users = buildTrojanUser(c.Tag, userInfo)
	} else if nodeInfo.NodeType == "Shadowsocks" {
		users = buildSSUser(c.Tag, userInfo)
	} else {
		return fmt.Errorf("Unsupported node type: %s", nodeInfo.NodeType)
	}
	err = c.addUsers(users, c.Tag)
	if err != nil {
		return err
	}
	log.Printf("Added %d new users", len(*userInfo))
	return nil
}

func compareUserList(old, new *[]api.UserInfo) (deleted, added []api.UserInfo) {
	msrc := make(map[api.UserInfo]byte) //按源数组建索引
	mall := make(map[api.UserInfo]byte) //源+目所有元素建索引

	var set []api.UserInfo //交集

	//1.源数组建立map
	for _, v := range *old {
		msrc[v] = 0
		mall[v] = 0
	}
	//2.目数组中，存不进去，即重复元素，所有存不进去的集合就是并集
	for _, v := range *new {
		l := len(mall)
		mall[v] = 1
		if l != len(mall) { //长度变化，即可以存
			l = len(mall)
		} else { //存不了，进并集
			set = append(set, v)
		}
	}
	//3.遍历交集，在并集中找，找到就从并集中删，删完后就是补集（即并-交=所有变化的元素）
	for _, v := range set {
		delete(mall, v)
	}
	//4.此时，mall是补集，所有元素去源中找，找到就是删除的，找不到的必定能在目数组中找到，即新加的
	for v := range mall {
		_, exist := msrc[v]
		if exist {
			deleted = append(deleted, v)
		} else {
			added = append(added, v)
		}
	}

	return deleted, added
}

func (c *Controller) userInfoMonitor() (err error) {
	// Get server status
	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		log.Print(err)
	}
	err = c.apiClient.ReportNodeStatus(
		&api.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		log.Print(err)
	}

	// Get User traffic
	userTraffic := make([]api.UserTraffic, 0)
	for _, user := range *c.userList {
		up, down := c.getTraffic(fmt.Sprintf("%s|%s|%d", c.Tag, user.Email, user.UID))
		if up > 0 || down > 0 {
			userTraffic = append(userTraffic, api.UserTraffic{
				UID:      user.UID,
				Email:    user.Email,
				Upload:   up,
				Download: down})
		}
	}
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(&userTraffic)
		if err != nil {
			log.Print(err)
		}
	}

	// Report Online info
	if onlineDevice, err := c.GetOnlineDevice(c.Tag); err != nil {
		log.Print(err)
	} else if len(*onlineDevice) > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(onlineDevice); err != nil {
			log.Print(err)
		} else {
			log.Printf("Report %d online users", len(*onlineDevice))
		}
	}
	// Report Illegal user
	if detectResult, err := c.GetDetectResult(c.Tag); err != nil {
		log.Print(err)
	} else if len(*detectResult) > 0 {
		if err = c.apiClient.ReportIllegal(detectResult); err != nil {
			log.Print(err)
		} else {
			log.Printf("Report %d illegal behaviors", len(*detectResult))
		}

	}
	return nil
}
