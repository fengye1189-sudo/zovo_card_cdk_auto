package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strconv"
	"time"
)

func LocalCardChoices(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	page, e := strconv.Atoi(c.DefaultQuery("page", "1"))
	if e != nil || page < 1 || page > 10000 {
		localError(c, 400, "页码不正确")
		return
	}
	if cardplatform.LoadConfig().APIKey == "" {
		localError(c, 503, "请先配置 Zovo API 密钥")
		return
	}
	cards, total, e := cardplatform.NewFromSettings().UnarchivedCardChoices(c.Request.Context(), page)
	if e != nil {
		localError(c, 502, "卡片列表暂时无法加载，请稍后重试；已选卡片不会被清空")
		return
	}
	settings := readLocalSettings()
	selected := map[int64]bool{}
	for _, id := range localCardIDs(settings) {
		selected[id] = true
	}
	list := []gin.H{}
	now := time.Now().Unix()
	for _, card := range cards {
		var busy int
		if e := db.DB.QueryRow("SELECT COALESCE(SUM(status IN ('reserved','review')),0) FROM local_cdks WHERE card_id=?", card.ID).Scan(&busy); e != nil {
			localError(c, 503, "使用次数暂时无法读取，请稍后刷新")
			return
		}
		kind, member, e := localCardKind(card.ID, selected[card.ID])
		if e != nil {
			localError(c, 503, "卡片周期暂时无法读取，请稍后刷新")
			return
		}
		cycle := localCardCycle{}
		if member {
			cycle, e = ensureLocalCardCycle(card.ID, kind, now)
			if e != nil {
				localError(c, 503, "卡片周期暂时无法读取，请稍后刷新")
				return
			}
		}
		remaining := -1
		cooling := false
		phase, retireState, reason := "", "", ""
		declines := 0
		if member {
			if e := db.DB.QueryRow("SELECT phase,decline_count,retire_state,retire_reason FROM automation_card_lifecycle WHERE card_id=?", card.ID).Scan(&phase, &declines, &retireState, &reason); e != nil {
				localError(c, 503, "卡片生命周期暂时无法读取，请稍后刷新")
				return
			}
		}
		list = append(list, gin.H{"id": card.ID, "last4": card.Last4, "product_code": card.Product, "status": card.Status, "balance_usd": card.Balance, "completed_count": cycle.Successes, "success_limit": 0, "remaining_uses": remaining, "retired": retireState != "" && retireState != "active", "cooling": cooling, "cooldown_until": 0, "card_kind": kind, "pool_member": member, "selected": selected[card.ID], "busy": busy > 0, "phase": phase, "decline_count": declines, "retire_state": retireState, "retire_reason": reason})
	}
	c.JSON(200, gin.H{"list": list, "total": total, "page": page, "page_size": 20})
}
