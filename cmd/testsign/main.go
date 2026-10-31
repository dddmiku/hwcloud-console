package main

import (
	"fmt"
	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
)

func main() {
	config.SetAccount("<ACCOUNT_A>")
	fmt.Println("CookieFile:", config.CookieFile())
	s, err := huawei.New("")
	if err != nil {
		fmt.Println("New err:", err)
		return
	}
	info, err := s.CheckLogin()
	fmt.Println("CheckLogin:", info, err)
}
