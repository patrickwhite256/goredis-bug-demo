package main

import (
	"log"
	"time"

	goredis "github.com/go-redis/redis"
)

func main() {
	client := goredis.NewClusterClient(&goredis.ClusterOptions{
		Addrs:           []string{"localhost:8988"},
		MinRetryBackoff: time.Millisecond * 200, // not necessary; used to help in reproduction
		ReadTimeout:     time.Millisecond * 500,
		ReadOnly:        true,
		MaxRetries:      3,
	})

	msg, err := client.Echo("message").Result()
	log.Println("msg:", msg)
	log.Println("err:", err)
}
