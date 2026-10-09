/**
 * 当前会话身份。
 *
 * 单独成模块是为了打破 store 之间的循环依赖：devices / transfers 需要知道
 * 「我是谁」来决定方向与权限，但不应反向依赖 session store。
 */

let meId = ''
let privileged = false
let deviceName = ''

export function getMeId(): string {
  return meId
}

export function setMeId(id: string) {
  meId = id
}

export function getDeviceName(): string {
  return deviceName
}

export function setDeviceName(name: string) {
  deviceName = name
}

export function isPrivileged(): boolean {
  return privileged
}

export function setPrivileged(v: boolean) {
  privileged = v
}
