package main

import (
	"database/sql"
	"fmt"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"ipd.org/containerfs/logger"
	"ipd.org/containerfs/utils"
	_ "ipd.org/containerfs/volmgr/mysql"
	"ipd.org/containerfs/volmgr/protobuf"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type RpcConfigOpts struct {
	ListenPort uint16 `gcfg:"listen-port"`
	ClientPort uint16 `gcfg:"client-port"`
}

var (
	dbhostip   = "127.0.0.1:3306"
	dbusername = "root"
	dbpassword = "root"
	dbname     = "containerfs"
)

var g_RpcConfig RpcConfigOpts
var Mutex sync.RWMutex
var err string
var blksize int32

type VolMgrServer struct{}

var VolMgrDB *sql.DB

func init() {
	var err error

	logger.SetConsole(true)
	logger.SetRollingFile("/var/log/containerfs", "volmgr.log", 10, 100, logger.MB) //each 100M rolling
	logger.SetLevel(logger.DEBUG)

	VolMgrDB, err = sql.Open("mysql", dbusername+":"+dbpassword+"@tcp("+dbhostip+")/"+dbname+"?charset=utf8")
	checkErr(err)

	err = VolMgrDB.Ping()
	checkErr(err)
	blksize = 10 //each blk size(G)
}

func checkErr(err error) {
	if err != nil {
		logger.Error("%s",err)
	}
}

func (s *VolMgrServer) DatanodeRegistry(ctx context.Context, in *protobuf.DatanodeRegistryReq) (*protobuf.DatanodeRegistryAck, error) {
	ack := protobuf.DatanodeRegistryAck{}
	dn_ip := utils.Inet_ntoa(in.Ip)
	ip := dn_ip.String()
	dn_port := in.Port
	dn_mount := in.MountPoint
	dn_capacity := in.Capacity

	disk, err := VolMgrDB.Prepare("INSERT INTO disks(ip,port,mount,total) VALUES(?, ?, ?, ?)")
	checkErr(err)
	defer disk.Close()

	_, err = disk.Exec(ip, dn_port, dn_mount, dn_capacity)
	checkErr(err)

	blkcount := dn_capacity / blksize

	hostip := ip
	hostport := strconv.Itoa(int(dn_port))
	blk, err := VolMgrDB.Prepare("INSERT INTO blk(hostip, hostport, allocated) VALUES(?, ?, ?)")
	checkErr(err)
	defer blk.Close()

	VolMgrDB.Exec("lock tables blk write")

	for i := int32(0); i < blkcount; i++ {
		blk.Exec(hostip, hostport, 0)
	}
	VolMgrDB.Exec("unlock tables")

	blkids := make([]int, 0)
	rows, err := VolMgrDB.Query("SELECT blkid FROM blk WHERE hostip = ? and hostport = ?", hostip, hostport)
	checkErr(err)
	defer rows.Close()
	var blkid int
	for rows.Next() {
		err := rows.Scan(&blkid)
		checkErr(err)
		blkids = append(blkids, blkid)
	}

	sort.Ints(blkids)
	logger.Debug("The disk(%s:%d) mount:%s have blks:%v", hostip, hostport, dn_mount, blkids)
	ack.StartBlockID = int32(blkids[0])
	ack.EndBlockID = int32(blkids[len(blkids)-1])
	ack.Ret = 0 //success
	return &ack, nil
}

func (s *VolMgrServer) DatanodeHeartbeat(ctx context.Context, in *protobuf.DatanodeHeartbeatReq) (*protobuf.DatanodeHeartbeatAck, error) {
	ack := protobuf.DatanodeHeartbeatAck{}
	port := in.Port
	used := in.Used
	free := in.Free
	statu := in.Status
	ipnr := utils.Inet_ntoa(in.Ip)
	ip := ipnr.String()

	logger.Debug("The disks(%s:%d) heartbeat info(used:%d -- free:%d -- statu:%d)", ip, port, used, free, statu)
	disk, err := VolMgrDB.Prepare("UPDATE disks SET used=?,free=?,statu=? WHERE ip=? and port=?")
	checkErr(err)
	defer disk.Close()
	_, err = disk.Exec(used, free, statu, ip, port)
	if err != nil {
		logger.Error("The disk(%s:%d) heartbeat update to db error:%s", ip, port, err)
		return &ack, nil
	}
	if statu != 0 {
		logger.Debug("The disk(%s:%d) bad statu:%d, so make it all blks is disabled", ip, port, statu)
		blk, err := VolMgrDB.Prepare("UPDATE blk SET disabled=1 WHERE hostip=? and hostport=?")
		checkErr(err)
		defer blk.Close()
		_, err = blk.Exec(ip, port)
		if err != nil {
			logger.Error("The disk(%s:%d) bad statu:%d update blk table disabled error:%s", ip, port, statu, err)
		}
	}
	return &ack, nil
}

func (s *VolMgrServer) CreateVol(ctx context.Context, in *protobuf.CreateVolReq) (*protobuf.CreateVolAck, error) {
	ack := protobuf.CreateVolAck{}
	volname := in.VolName
	volsize := in.SpaceQuota
	voluuid, err := utils.GenUUID()

	//the volume need block group total numbers
	var blkgrpnum int32
	if volsize < blksize {
		blkgrpnum = 1
	} else if volsize%blksize == 0 {
		blkgrpnum = volsize / blksize
	} else {
		blkgrpnum = volsize/blksize + 1
	}

	// insert the volume info to volumes tables
	vol, err := VolMgrDB.Prepare("INSERT INTO volumes(uuid, name, size) VALUES(?, ?, ?)")
	if err != nil {
		logger.Error("Create volume(%s -- %s) insert db error:%s", volname, voluuid, err)
		ack.Ret = 1 // db error
		return &ack, nil
	}
	defer vol.Close()
	vol.Exec(voluuid, volname, volsize)

	//allocate block group for the volume
	for i := int32(0); i < blkgrpnum; i++ {
		rows, err := VolMgrDB.Query("SELECT blkid FROM blk WHERE allocated = 0 group by hostip ORDER BY rand() LIMIT 3 FOR UPDATE")
		if err != nil {
			logger.Error("Create volume(%s -- %s) select blk for the %dth blkgroup error:%s", volname, voluuid, i, err)
			ack.Ret = 1
			return &ack, nil
		}
		defer rows.Close()

		var blkid int
		var blks string = ""
		for rows.Next() {
			err := rows.Scan(&blkid)
			if err != nil {
				ack.Ret = 1
				return &ack, nil
			}

			//tx, err := VolMgrDB.Begin()
			//defer tx.Rollback()
			blk, err := VolMgrDB.Prepare("UPDATE blk SET allocated=1 WHERE blkid=?")
			if err != nil {
				logger.Error("update blk:%d have allocated error:%s", blkid)
				ack.Ret = 1
				return &ack, nil
			}
			defer blk.Close()
			_, err = blk.Exec(blkid)
			if err != nil {
				ack.Ret = 1
				return &ack, nil
			}
			//err = tx.Commit()
			blks = blks + strconv.Itoa(blkid) + ","
		}

		logger.Debug("The volume(%s -- %s) one blkgroup have blks:%s", volname, voluuid, blks)
		blkgrp, err := VolMgrDB.Prepare("INSERT INTO blkgrp(blks, volume_uuid) VALUES(?, ?)")
		if err != nil {
			ack.Ret = 1
			return &ack, nil
		}
		defer blkgrp.Close()
		blkgrp.Exec(blks, voluuid)
	}

	ack.Ret = 0 //success
	ack.UUID = voluuid
	return &ack, nil
}

func (s *VolMgrServer) GetVolInfo(ctx context.Context, in *protobuf.GetVolInfoReq) (*protobuf.GetVolInfoAck, error) {
	ack := protobuf.GetVolInfoAck{}
	var volInfo protobuf.VolInfo

	voluuid := in.UUID

	var name string
	var size int32
	vols, err := VolMgrDB.Query("SELECT name,size FROM volumes WHERE uuid = ?", voluuid)
	if err != nil {
		logger.Error("Get volume(%s) from db error:%s", voluuid, err)
		ack.Ret = 1
		return &ack, nil
	}
	defer vols.Close()
	for vols.Next() {
		err = vols.Scan(&name, &size)
		if err != nil {
			ack.Ret = 1
			return &ack, nil
		}
		volInfo.VolID = voluuid
		volInfo.VolName = name
		volInfo.SpaceQuota = size
	}

	var blkgrpid int
	var blks string
	blkgrp, err := VolMgrDB.Query("SELECT blkgrpid,blks FROM blkgrp WHERE volume_uuid = ?", voluuid)
	if err != nil {
		logger.Error("Get blkgroups for volume(%s) error:%s", voluuid, err)
		ack.Ret = 1
		return &ack, nil
	}
	defer blkgrp.Close()
	pBlockGroups := []*protobuf.BlockGroup{}
	for blkgrp.Next() {
		err := blkgrp.Scan(&blkgrpid, &blks)
		if err != nil {
			ack.Ret = 1
			return &ack, nil
		}
		logger.Debug("Get blks:%s in blkgroup:%d for volume(%s)", blks, blkgrpid, voluuid)
		blkids := strings.Split(blks, ",")

		pBlockInfos := []*protobuf.BlockInfo{}
		for _, ele := range blkids {
			if ele == "," {
				continue
			}
			blkid, err := strconv.Atoi(ele)
			var hostip string
			var hostport int
			blk, err := VolMgrDB.Query("SELECT hostip,hostport FROM blk WHERE blkid = ?", blkid)
			if err != nil {
				logger.Error("Get each blk:%d on which host error:%s for volume(%s)", blkid, err, voluuid)
				ack.Ret = 1
				return &ack, nil
			}
			defer blk.Close()
			for blk.Next() {
				err = blk.Scan(&hostip, &hostport)
				if err != nil {
					ack.Ret = 1
					return &ack, nil
				}
				tmpBlockInfo := protobuf.BlockInfo{}
				tmpBlockInfo.BlockID = int32(blkid)
				ipnr := net.ParseIP(hostip)
				ipint := utils.Inet_aton(ipnr)
				tmpBlockInfo.DataNodeIP = ipint
				tmpBlockInfo.DataNodePort = int32(hostport)
				pBlockInfos = append(pBlockInfos, &tmpBlockInfo)
			}
		}
		tmpBlockGroup := protobuf.BlockGroup{}
		tmpBlockGroup.BlockGroupID = int32(blkgrpid)
		tmpBlockGroup.BlockInfos = pBlockInfos
		pBlockGroups = append(pBlockGroups, &tmpBlockGroup)
	}
	volInfo.BlockGroups = pBlockGroups
	logger.Debug("Get info:%v for the volume(%s)", volInfo, voluuid)
	ack = protobuf.GetVolInfoAck{Ret: 0, VolInfo: &volInfo}
	return &ack, nil
}

func StartVolMgrService() {
	g_RpcConfig.ListenPort = 10001
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", g_RpcConfig.ListenPort))
	if err != nil {
		panic(fmt.Sprintf("Failed to listen on:%v", g_RpcConfig.ListenPort))
	}
	s := grpc.NewServer()
	protobuf.RegisterVolMgrServer(s, &VolMgrServer{})
	// Register reflection service on gRPC server.
	reflection.Register(s)
	if err := s.Serve(lis); err != nil {
		panic("Failed to serve")
	}
}

func main() {

	//for multi-cpu scheduling
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)

	defer VolMgrDB.Close()
	StartVolMgrService()
}
