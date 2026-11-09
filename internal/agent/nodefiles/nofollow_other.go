//go:build !linux

package nodefiles

// oNoFollow 在其他系统（开发机）上不用：节点只支持 Linux，打开后照样由 verifyOpened 核对。
const oNoFollow = 0
