package probe

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"qqtang/internal/protocol/game"
	"qqtang/internal/server/application"
)

// handleShopCatalogMessage handles type-3 traffic on the shop endpoint. Type 3
// and type 2 use independent client TCP objects, but EnterShop supplies both
// objects with the same QQTDir-advertised endpoint.
func (server *Server) handleShopCatalogMessage(session *connectionSession, connectionID string, data []byte) ([]byte, string, bool) {
	request, err := game.DecodeLocalShopListRequest(data)
	if err != nil {
		return nil, "", false
	}
	server.log(logEvent{
		Level: "info", Event: "shop_list_requested", ConnectionID: connectionID,
		AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ShopListCommand),
		Result: fmt.Sprintf("client_%d_cs_%d_catalog_%d", request.ClientVersion, request.CSVersion, request.ShopListVersion),
	})
	if server.shopCatalog == nil {
		err = fmt.Errorf("shop catalog is unavailable")
	} else if err = server.bindAuxiliaryShopConnection(session, request.UIN); err == nil {
		version := server.shopCatalog.Version()
		response, buildErr := game.BuildLocalShopListCurrentWithReader(data, version, rand.Reader)
		if buildErr == nil {
			return response, fmt.Sprintf("qqt_shop_list_current_%d_client_%d", version, request.ShopListVersion), true
		}
		err = buildErr
	}
	server.log(logEvent{
		Level: "warn", Event: "shop_list_request_rejected", ConnectionID: connectionID,
		AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ShopListCommand),
		Result: "rejected", ErrorContext: err.Error(),
	})
	return nil, "qqt_shop_list_rejected", true
}

// handleShopType2SessionMessage handles purchases from the type-2 connection
// on the same QQTDir-advertised shop endpoint. Configuration messages remain
// owned by the login/config dispatcher.
func (server *Server) handleShopType2SessionMessage(session *connectionSession, connectionID string, data []byte) ([]byte, string, bool) {
	request, err := game.DecodeLocalShopBuyRequest(data)
	if err != nil {
		return nil, "", false
	}
	purchaseResponse, outcome, settleErr := server.settleShopPurchaseLocked(session, request)
	if settleErr != nil {
		purchaseResponse.ResultID = game.ShopBuyResultFailed
		purchaseResponse.ResultString = "购买失败，请稍后重试"
		server.log(logEvent{
			Level: "error", Event: "shop_purchase_failed", ConnectionID: connectionID,
			AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ShopBuyCommand),
			Result: outcome, ErrorContext: settleErr.Error(),
		})
	}
	response, buildErr := game.BuildLocalShopBuyResponse(data, purchaseResponse)
	if buildErr != nil {
		server.log(logEvent{
			Level: "error", Event: "shop_purchase_response_failed", ConnectionID: connectionID,
			AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ShopBuyCommand),
			Result: outcome, ErrorContext: buildErr.Error(),
		})
		return nil, "qqt_shop_purchase_response_failed", true
	}
	level := "info"
	if purchaseResponse.ResultID != game.ShopBuyResultSuccess {
		level = "warn"
	}
	serverVersion := uint32(0)
	if server.shopCatalog != nil {
		serverVersion = server.shopCatalog.Version()
	}
	server.log(logEvent{
		Level: level, Event: "shop_purchase_settled", ConnectionID: connectionID,
		AccountID: fmt.Sprint(request.UIN), MessageID: fmt.Sprintf("0x%04X", game.ShopBuyCommand),
		Result: fmt.Sprintf("%s_pay_type_%d_mixed_%d_commodity_%d_client_%d_request_catalog_%d_server_catalog_%d",
			outcome, request.PayType, request.AgreeMixedPayment, request.CommodityID,
			request.ClientVersion, request.CommodityVersion, serverVersion),
	})
	if purchaseResponse.ResultID == game.ShopBuyResultSuccess && len(purchaseResponse.ItemIDs) != 0 {
		if item, ok := inventoryItemByID(session.Profile.Inventory, uint16(purchaseResponse.ItemIDs[0])); ok {
			// RESPONSE_BUY settles the transaction and refreshes the displayed
			// balance. The profile-owned inventory is a separate QQTSection
			// projection and is refreshed by NOTIFY_PLAYER_ITEMADD. Send it only
			// after the purchase response has had time to clear QQTShop's native
			// busy lock, and deliver it on the primary hall connection rather
			// than the disposable shop socket.
			server.schedulePurchasedItemRefresh(request.UIN, request.CommodityID, item)
		}
	}
	return response, "qqt_shop_purchase_" + outcome, true
}

func inventoryItemByID(inventory []game.ItemInfo, itemID uint16) (game.ItemInfo, bool) {
	for _, item := range inventory {
		if item.ItemID == itemID {
			return item, true
		}
	}
	return game.ItemInfo{}, false
}

func (server *Server) schedulePurchasedItemRefresh(uin, commodityID uint32, item game.ItemInfo) {
	server.runAfter(100*time.Millisecond, func() {
		server.sendPrimaryInventoryItemRefresh(uin, commodityID, item)
	})
}

func (server *Server) scheduleInventoryStatusRefresh(uin uint32, items []game.ItemInfo) {
	if len(items) == 0 {
		return
	}
	absolute := append([]game.ItemInfo(nil), items...)
	server.runAfter(100*time.Millisecond, func() {
		server.sendPrimaryInventoryItemsRefresh(uin, 0, absolute)
	})
}

func (server *Server) sendPrimaryInventoryItemRefresh(uin, commodityID uint32, item game.ItemInfo) {
	server.sendPrimaryInventoryItemsRefresh(uin, commodityID, []game.ItemInfo{item})
}

func (server *Server) sendPrimaryInventoryItemsRefresh(uin, commodityID uint32, items []game.ItemInfo) {
	if len(items) == 0 {
		return
	}
	server.liveMu.RLock()
	var primary *connectionSession
	for _, candidate := range server.liveSessions {
		// Auxiliary shop sockets deliberately keep liveUIN at zero, so the
		// atomic owner identity alone distinguishes the primary hall session.
		if candidate.liveUIN.Load() != uin || candidate.connection == nil {
			continue
		}
		primary = candidate
		break
	}
	server.liveMu.RUnlock()
	if primary == nil {
		server.log(logEvent{
			Level: "warn", Event: "shop_inventory_refresh_skipped", AccountID: fmt.Sprint(uin),
			Result: fmt.Sprintf("items_%d", len(items)), ErrorContext: "primary hall session is unavailable",
		})
		return
	}
	template := primary.packetTemplate()
	if len(template) == 0 {
		server.log(logEvent{
			Level: "warn", Event: "shop_inventory_refresh_skipped", ConnectionID: primary.connectionID,
			AccountID: fmt.Sprint(uin), Result: fmt.Sprintf("items_%d", len(items)),
			ErrorContext: "primary hall session has no packet template",
		})
		return
	}
	packet, err := buildInventoryItemsRefreshPacket(template, uin, commodityID, items)
	if err != nil {
		server.log(logEvent{
			Level: "error", Event: "shop_inventory_refresh_build_failed", ConnectionID: primary.connectionID,
			AccountID: fmt.Sprint(uin), Result: fmt.Sprintf("items_%d", len(items)), ErrorContext: err.Error(),
		})
		return
	}
	server.writeTCP(primary.connection, primary.connectionID, primary.localAddress, primary.remoteAddress,
		packet, fmt.Sprintf("qqt_shop_inventory_items_%d_absolute", len(items)))
}

func buildInventoryItemsRefreshPacket(template []byte, uin, commodityID uint32, items []game.ItemInfo) ([]byte, error) {
	return game.BuildLocalPlayerItemAddNotification(template, game.PlayerItemAddNotification{
		UIN: uin, Time: uint32(time.Now().Unix()), SourceUIN: uin,
		CommodityID: commodityID, Items: items,
	})
}

// settleShopPurchaseLocked requires the caller to hold session.mu. The TCP
// dispatcher already does so for the complete decode/settle/respond cycle.
func (server *Server) settleShopPurchaseLocked(session *connectionSession, request game.ShopBuyRequest) (game.ShopBuyResponse, string, error) {
	response := game.ShopBuyResponse{
		ResultID: game.ShopBuyResultFailed, DealType: request.DealType,
		PayType: request.PayType, CommodityID: request.CommodityID,
	}
	if session == nil {
		return response, "rejected", fmt.Errorf("shop purchase session is nil")
	}
	if session.UIN == 0 {
		if err := server.bindAuxiliaryShopConnection(session, request.UIN); err != nil {
			return response, "rejected", err
		}
	}
	if session.UIN != request.UIN {
		return response, "rejected", fmt.Errorf("shop purchase UIN %d does not match session UIN %d", request.UIN, session.UIN)
	}
	if server.shopCatalog == nil || server.playerStore == nil {
		return response, "rejected", fmt.Errorf("shop catalog or player store is unavailable")
	}
	if request.DealType != game.ShopDealTypePurchase {
		response.ResultString = "当前购买方式暂不支持"
		return response, fmt.Sprintf("unsupported_deal_type_%d", request.DealType), nil
	}
	players, err := server.players()
	if err != nil {
		return response, "persistence_failed", err
	}
	purchase, err := players.PurchaseCommodity(context.Background(), request.UIN, server.shopCatalog, request.CommodityID,
		applicationPaymentMethod(request.PayType), request.AgreeMixedPayment != 0)
	if err != nil {
		return response, "persistence_failed", err
	}
	response.ResultString = purchase.Message
	if purchase.Product.CommodityID != 0 && request.PayType == game.ShopPayTypeGameMoney {
		response.PayMoney = purchase.Product.SugarPrice
	}
	switch purchase.Status {
	case application.PurchaseUnknownCommodity:
		return response, "unknown_commodity", nil
	case application.PurchaseUnsupportedMethod:
		return response, fmt.Sprintf("unsupported_pay_type_%d", request.PayType), nil
	case application.PurchaseExternalFunds:
		return response, fmt.Sprintf("external_funds_unavailable_pay_type_%d", request.PayType), nil
	case application.PurchaseInsufficientFunds:
		return response, "insufficient_game_money", nil
	case application.PurchaseAlreadyOwned:
		return response, "already_owned", nil
	case application.PurchaseSuccess:
	default:
		return response, "persistence_failed", fmt.Errorf("unknown application purchase status %q", purchase.Status)
	}
	// handleTCP serializes every message for this connection under session.mu.
	// Do not lock it again here: sync.Mutex is not re-entrant, and only the
	// successful purchase path reaches this profile replacement.
	session.replaceProfile(purchase.Profile)
	server.syncPrimarySessionProfile(session, request.UIN, session.Profile)

	response.ResultID = game.ShopBuyResultSuccess
	response.ItemIDs = []uint32{uint32(purchase.Product.ItemID)}
	response.TicketLeft = purchase.BalanceLeft
	return response, fmt.Sprintf("success_item_%d_money_%d", purchase.Product.ItemID, purchase.BalanceLeft), nil
}

func applicationPaymentMethod(payType game.ShopPaymentMethod) application.PaymentMethod {
	switch payType {
	case game.ShopPayTypeQCoin:
		return application.PaymentQCoin
	case game.ShopPayTypeQPoint:
		return application.PaymentQPoint
	case game.ShopPayTypeGameMoney:
		return application.PaymentGameMoney
	case game.ShopPayTypeKubiGem:
		return application.PaymentKubiGem
	case game.ShopPayTypeVNet:
		return application.PaymentVNet
	default:
		return application.PaymentMethod(0xFF)
	}
}
