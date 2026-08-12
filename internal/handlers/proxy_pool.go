package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultProxyPoolSetting = "default_proxy_pool_id"

var errEmptyDefault = errors.New("默认代理池至少需要一个代理")

func proxyPoolLines(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })
	lines := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		line := strings.TrimSpace(part)
		if line == "" {
			continue
		}
		if _, exists := seen[line]; exists {
			continue
		}
		if _, err := newProxyHTTPClient(line, 0); err != nil {
			return nil, fmt.Errorf("代理格式错误：第 %d 条", len(lines)+1)
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	if len(lines) > 10000 {
		return nil, fmt.Errorf("单个代理池最多 10000 条代理")
	}
	return lines, nil
}

func prepareProxyPool(name, raw string) (string, string, int, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return "", "", 0, fmt.Errorf("代理池名称需为 1 到 64 个字符")
	}
	lines, err := proxyPoolLines(raw)
	if err != nil {
		return "", "", 0, err
	}
	return name, strings.Join(lines, "\n"), len(lines), nil
}

func defaultProxyPoolIDFrom(db *gorm.DB) uint {
	var setting models.Setting
	if db.Where("key = ?", defaultProxyPoolSetting).First(&setting).Error != nil {
		return 0
	}
	id, _ := strconv.ParseUint(strings.TrimSpace(setting.Value), 10, 64)
	return uint(id)
}

func (h *Handler) defaultProxyPoolID() uint {
	return defaultProxyPoolIDFrom(h.DB)
}

func (h *Handler) setDefaultProxyPoolID(tx *gorm.DB, id uint) error {
	setting := models.Setting{Key: defaultProxyPoolSetting, Value: strconv.FormatUint(uint64(id), 10)}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&setting).Error
}

func (h *Handler) ProxyPoolList(c *gin.Context) {
	var pools []models.ProxyPool
	if err := h.DB.Order("name, id").Find(&pools).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range pools {
		lines, _ := proxyPoolLines(pools[i].Proxies)
		pools[i].ProxyCount = len(lines)
		pools[i].Proxies = ""
	}
	c.JSON(http.StatusOK, gin.H{"data": pools, "default_proxy_pool_id": h.defaultProxyPoolID()})
}

func (h *Handler) ProxyPoolGet(c *gin.Context) {
	var pool models.ProxyPool
	if err := h.DB.First(&pool, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "代理池不存在"})
		return
	}
	lines, _ := proxyPoolLines(pool.Proxies)
	pool.ProxyCount = len(lines)
	c.JSON(http.StatusOK, pool)
}

func (h *Handler) ProxyPoolCreate(c *gin.Context) {
	var input struct {
		Name    string `json:"name"`
		Proxies string `json:"proxies"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name, proxies, count, err := prepareProxyPool(input.Name, input.Proxies)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var existing int64
	h.DB.Model(&models.ProxyPool{}).Where("LOWER(name) = ?", strings.ToLower(name)).Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "代理池名称已存在"})
		return
	}
	pool := models.ProxyPool{Name: name, Proxies: proxies, ProxyCount: count}
	if err := h.DB.Create(&pool).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "代理池名称已存在"})
		return
	}
	c.JSON(http.StatusCreated, pool)
}

func (h *Handler) ProxyPoolUpdate(c *gin.Context) {
	var pool models.ProxyPool
	if err := h.DB.First(&pool, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "代理池不存在"})
		return
	}
	var input struct {
		Name      string `json:"name"`
		Proxies   string `json:"proxies"`
		IsDefault *bool  `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name, proxies, count, err := prepareProxyPool(input.Name, input.Proxies)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var existing int64
	h.DB.Model(&models.ProxyPool{}).Where("LOWER(name) = ? AND id <> ?", strings.ToLower(name), pool.ID).Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "代理池名称已存在"})
		return
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		currentDefaultID := defaultProxyPoolIDFrom(tx)
		if count == 0 && ((input.IsDefault != nil && *input.IsDefault) || (input.IsDefault == nil && currentDefaultID == pool.ID)) {
			return errEmptyDefault
		}
		if err := tx.Model(&pool).Updates(map[string]any{"name": name, "proxies": proxies}).Error; err != nil {
			return err
		}
		if input.IsDefault == nil {
			return nil
		}
		defaultID := currentDefaultID
		if *input.IsDefault {
			defaultID = pool.ID
		} else if currentDefaultID == pool.ID {
			defaultID = 0
		}
		return h.setDefaultProxyPoolID(tx, defaultID)
	})
	if err != nil {
		if errors.Is(err, errEmptyDefault) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusConflict, gin.H{"error": "代理池名称已存在"})
		}
		return
	}
	pool.Name, pool.Proxies, pool.ProxyCount = name, proxies, count
	c.JSON(http.StatusOK, pool)
}

func (h *Handler) ProxyPoolDelete(c *gin.Context) {
	var pool models.ProxyPool
	if err := h.DB.First(&pool, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "代理池不存在"})
		return
	}
	if err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&pool).Error; err != nil {
			return err
		}
		if defaultProxyPoolIDFrom(tx) == pool.ID {
			return h.setDefaultProxyPoolID(tx, 0)
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) ProxyPoolSetDefault(c *gin.Context) {
	var input struct {
		ProxyPoolID uint `json:"proxy_pool_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.ProxyPoolID != 0 {
		var pool models.ProxyPool
		if err := h.DB.First(&pool, input.ProxyPoolID).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "代理池不存在"})
			return
		}
		lines, err := proxyPoolLines(pool.Proxies)
		if err != nil || len(lines) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "默认代理池至少需要一个代理"})
			return
		}
	}
	if err := h.setDefaultProxyPoolID(h.DB, input.ProxyPoolID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "default_proxy_pool_id": input.ProxyPoolID})
}
