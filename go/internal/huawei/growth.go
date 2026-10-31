package huawei

import (
	"fmt"
	"sort"
	"strings"
)

var StarLabel = map[int]string{1: "1星", 2: "2星", 3: "3星", 4: "4星"}

type TaskState struct {
	Degree   int
	Title    string
	Finished bool
	Bonus    int
}

type Summary struct {
	User       string
	Beans      int
	Signed     bool
	Continuity int
	SignCount  int
	Tasks      []TaskState
}

func TasksByStar(s *Session, stars ...int) ([]Mission, error) {
	all, err := s.AllMissions()
	if err != nil {
		return nil, err
	}
	set := map[int]bool{}
	for _, star := range stars {
		set[star] = true
	}
	var out []Mission
	for _, m := range all {
		if set[m.Degree] {
			out = append(out, m)
		}
	}
	return out, nil
}

func Summarize(s *Session, stars ...int) (*Summary, error) {
	if _, err := s.RequireLogin(); err != nil {
		return nil, err
	}
	status, err := s.SignStatus()
	if err != nil {
		return nil, err
	}
	missions, err := TasksByStar(s, stars...)
	if err != nil {
		return nil, err
	}
	sum := &Summary{
		User:       s.Username,
		Beans:      s.Beans(),
		Signed:     asFloat(status["sign_flag"]) == 1,
		Continuity: int(asFloat(status["sign_continuity_count"])),
		SignCount:  int(asFloat(status["sign_count"])),
	}
	for _, m := range missions {
		sum.Tasks = append(sum.Tasks, TaskState{
			Degree:   m.Degree,
			Title:    m.Title,
			Finished: m.IsFinish == 1,
			Bonus:    m.Bonus,
		})
	}
	return sum, nil
}

func FormatSummary(s *Summary) string {
	var b strings.Builder
	signText := "今日未签到"
	if s.Signed {
		signText = "今日已签到"
	}
	fmt.Fprintf(&b, "账号: %s    积分: %d\n", s.User, s.Beans)
	fmt.Fprintf(&b, "签到: %s（连续 %d 天，累计 %d 次）\n\n", signText, s.Continuity, s.SignCount)
	degrees := map[int]bool{}
	for _, t := range s.Tasks {
		degrees[t.Degree] = true
	}
	var ordered []int
	for d := range degrees {
		ordered = append(ordered, d)
	}
	sort.Ints(ordered)
	for _, d := range ordered {
		group := 0
		undone := 0
		for _, t := range s.Tasks {
			if t.Degree == d {
				group++
				if !t.Finished {
					undone++
				}
			}
		}
		label := StarLabel[d]
		if label == "" {
			label = fmt.Sprintf("%d星", d)
		}
		fmt.Fprintf(&b, "%s任务 %d 个，未完成 %d 个\n", label, group, undone)
		for _, t := range s.Tasks {
			if t.Degree != d {
				continue
			}
			mark := "未完成"
			if t.Finished {
				mark = "已完成"
			}
			fmt.Fprintf(&b, "  [%s] %s (+%d)\n", mark, t.Title, t.Bonus)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}