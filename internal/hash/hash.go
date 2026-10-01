// Package hash 提供按客户端 ID 的稳定分桶。
//
// 分桶契约一旦发布即固定：bucket = FNV-1a/64(clientID) mod 100，取值 0..99。
// 选择纯算法（无随机盐、无状态）是为了：
//   - 多个 API 实例算出同一个桶；
//   - 实例重启后分组结果不变；
//   - 比例调整只移动阈值，不重排客户端。
package hash

import "hash/fnv"

// BucketCount 是百分比发布的总桶数（0～100%）。
const BucketCount = 100

// Bucket 返回客户端 ID 落入的稳定桶号 0..99。
func Bucket(clientID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(clientID))
	return h.Sum64() % BucketCount
}

// InTrial 判断桶号是否落在试用比例内。
// 规则：trial_percent=0 无人进入；=100 全部进入（bucket<=99 恒真）。
func InTrial(bucket uint64, trialPercent int) bool {
	return int(bucket) < trialPercent
}
