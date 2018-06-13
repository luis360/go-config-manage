// main
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/samuel/go-zookeeper/zk"
)

// 配置管理系统根节点
var rootNode string = "/config-manage"

// 业务系统名称
var appName string

// 业务节点
var appNode string

// 业务代码机器路径
var appPath string

//配置模板文件
var configExamples []string

//更改配置存储器
var configExampleContents map[string]string

//原始配置存储器
var originConfigExample map[string]string

var mu sync.Mutex

func main() {
	// 初始化
	newConfigContents()

	// 创建zk连接
	conn := connect()
	defer conn.Close()

	// 创建根节点
	_, err := createNode(conn, rootNode)
	if err != nil {
		panic(err)
	}

	// 获取业务系统名
	getAppParams()

	//创建业务节点
	_, err = createNode(conn, appNode)
	if err != nil {
		panic(err)
	}

	// 遍历配置文件
	scanFileList()

	// 监控业务系统节点
	mirror(conn, appNode)

}

// 初始化配置存储器
func newConfigContents() {
	configExampleContents = make(map[string]string)
	originConfigExample = make(map[string]string)
}

// 错误判断
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// zk连接
func connect() *zk.Conn {
	servers := strings.Split("127.0.0.1:2181", ",")
	fmt.Println(servers)
	conn, _, err := zk.Connect(servers, time.Second)
	must(err)
	return conn
}

// 监控
func mirror(conn *zk.Conn, path string) {
	snapshots, nodeSnapshots, errors := mirrorNode(conn, path)
	for {
		select {
		case snapshot := <-snapshots:
			//fmt.Printf("path:%v, child:%v\n", path, snapshot)
			for key, nodes := range snapshot {
				for i := 0; i < len(nodes); i++ {
					path = key + "/" + nodes[i]
					go mirror(conn, path)
				}
			}

		case nodeSnapshot := <-nodeSnapshots:
			//fmt.Printf("path:%v, data:%v\n", path, nodeSnapshot)
			upConfigFiles(path, nodeSnapshot[path])

		case err := <-errors:
			fmt.Printf("wow failed: %+v \n", err)
		}

	}
}

//监控节点新增或删除
func mirrorNode(conn *zk.Conn, path string) (chan map[string][]string, chan map[string]string, chan map[string]error) {
	snapshots := make(chan map[string][]string)
	errors := make(chan map[string]error)

	go func() {
		for {
			snapshot, _, events, err := conn.ChildrenW(path)
			if err != nil {
				errors <- map[string]error{
					path: err,
				}
				return
			}
			snapshots <- map[string][]string{
				path: snapshot,
			}
			evt := <-events
			if evt.Err != nil {
				errors <- map[string]error{
					path: evt.Err,
				}
				return
			}
		}
	}()

	nodeSnapshots := make(chan map[string]string)
	go func() {
		for {
			nodeBytes, _, events, err := conn.GetW(path)
			if err != nil {
				errors <- map[string]error{
					path: err,
				}
				return
			}
			nodeData := map[string]string{
				path: string(nodeBytes),
			}
			nodeSnapshots <- nodeData
			evt := <-events
			if evt.Err != nil {
				nodeErr := map[string]error{
					path: evt.Err,
				}
				errors <- nodeErr
				return
			}
		}
	}()

	return snapshots, nodeSnapshots, errors
}

// 创建配置管理系统节点
func createNode(conn *zk.Conn, path string) (bool, error) {
	exist, _, err := conn.Exists(path)
	if err != nil {
		fmt.Printf("create node[%s] fail\n", path)
		return exist, err
	}
	if !exist {
		acl := zk.WorldACL(zk.PermAll)
		_, err = conn.Create(path, []byte("this is root"), int32(0), acl)
		if err != nil {
			fmt.Printf("create node[%s] fail\n", path)
			return exist, err
		}
		exist = true
	}
	return exist, err
}

// 获取app名
func getAppParams() {

	var (
		name = flag.String("app", "app1", "this is a application name")
		path = flag.String("path", "", "this is application path")
	)

	flag.Parse()

	appName = *name
	appNode = rootNode + "/" + appName

	appPath = *path
	if len(appPath) == 0 {
		panic("please input application path")
	}
	fmt.Printf("appname:%s, appNode:%s, appPath:%s\n", appName, appNode, appPath)
}

// 遍历appNode下的文件
func scanFileList() {
	err := filepath.Walk(appPath, func(path string, info os.FileInfo, err error) error {
		if info == nil {
			return nil
		}
		if !info.IsDir() {
			if strings.Contains(path, "example") {
				configExamples = append(configExamples, path)
			}
		}
		return nil
	})
	if err != nil {
		fmt.Printf("filepath.walk() error:%v\n", err)
	}
	return
}

// 配置替换
func upConfigFiles(node string, data string) {
	nodes := strings.Split(node, "/")
	key := "{#" + nodes[len(nodes)-1] + "#}"
	for i := 0; i < len(configExamples); i++ {
		example := configExamples[i]
		rs, err := upFile(example, key, data)
		if err != nil {
			fmt.Printf("upconfig error:%v\n", err)
		}
		if rs {
			break
		}
	}
}

//更新文件
func upFile(example string, key string, data string) (bool, error) {

	mu.Lock()
	defer mu.Unlock()

	var info string
	var originInfo string
	if _, ok := configExampleContents[example]; ok {
		info = configExampleContents[example]
		originInfo = originConfigExample[example]
	} else {
		fi, err := os.Open(example)
		if err != nil {
			return false, err
		}
		defer fi.Close()
		buffBytes, err := ioutil.ReadAll(fi)
		if err != nil {
			return false, err
		}
		info = string(buffBytes)
		originInfo = info
		originConfigExample[example] = originInfo
	}

	if !strings.Contains(originInfo, key) {
		return false, nil
	}

	re1, _ := regexp.Compile("(\\w+?=)" + "(" + key + ")")
	rs1 := re1.FindAllString(originInfo, -1)

	re2, _ := regexp.Compile(key)
	rs2 := re2.ReplaceAllString(rs1[0], "")

	re3, _ := regexp.Compile(rs2 + "(.*)")
	c := re3.ReplaceAllString(info, rs2+data)
	fmt.Printf("reg:%v\n", c)

	filename := strings.Replace(example, ".example", "", -1)
	_, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, err
		}
	}
	configExampleContents[example] = c

	ioutil.WriteFile(filename, []byte(c), os.ModePerm)
	return true, nil
}
