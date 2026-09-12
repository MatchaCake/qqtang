package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"qqtang/internal/game/equipment"
	"qqtang/internal/game/itemcatalog"
	"qqtang/internal/game/shopcatalog"
	"qqtang/internal/protocol/capture"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func TestInventoryStatusRefreshUsesPrimaryHallConnectionForStack(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	const uin uint32 = 1_000_001
	primary := &connectionSession{
		connection: serverSide, connectionID: "hall", localAddress: "127.0.0.1:18000",
		remoteAddress: "127.0.0.1:50000",
	}
	primary.liveUIN.Store(uin)
	template := testLocalRoutedPacket(t, 0x0107, 4, 0xFFFF, 0, uin)
	primary.notePacket(template)
	captureWriter, err := capture.Open(filepath.Join(t.TempDir(), "capture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer captureWriter.Close()
	server := &Server{liveSessions: map[net.Conn]*connectionSession{serverSide: primary}, capture: captureWriter, logWriter: io.Discard}

	read := make(chan []byte, 1)
	go func() {
		lengthPrefix := make([]byte, 4)
		if _, err := io.ReadFull(clientSide, lengthPrefix); err != nil {
			read <- nil
			return
		}
		packet := make([]byte, binary.BigEndian.Uint32(lengthPrefix))
		copy(packet, lengthPrefix)
		if _, err := io.ReadFull(clientSide, packet[4:]); err != nil {
			read <- nil
			return
		}
		read <- packet
	}()

	want := game.NewPermanentItemInfo(2361, 3)
	want.ItemStatus = game.ItemStatusCollected
	server.sendPrimaryInventoryItemsRefresh(uin, 0, []game.ItemInfo{want})
	packet := <-read
	if len(packet) == 0 {
		t.Fatal("primary hall connection received no inventory refresh")
	}
	inspection, err := game.InspectLocalPacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Command != game.PlayerItemAddNotifyCommand {
		t.Fatalf("inventory refresh command = 0x%04X", inspection.Command)
	}
	refresh, err := game.ParsePlayerItemAddNotification(inspection.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.UIN != uin || refresh.CommodityID != 0 || len(refresh.Items) != 1 || refresh.Items[0] != want {
		t.Fatalf("inventory refresh = %+v", refresh)
	}
}

func TestCollectionStatusRefreshPrecedesSuccessResponse(t *testing.T) {
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(2361, 7)}
	if err := store.Save(context.Background(), uin, profile); err != nil {
		t.Fatal(err)
	}
	server := &Server{playerStore: store, logWriter: io.Discard}
	session := &connectionSession{UIN: uin, Profile: profile}
	config := ListenerConfig{Response: ResponseConfig{QQTGameBegin: true}}

	for _, transition := range []struct {
		name       string
		newStatus  byte
		wantStatus byte
	}{
		{name: "collect stack", newStatus: game.ItemStatusCollected, wantStatus: game.ItemStatusCollected},
		{name: "restore stack", newStatus: game.ItemStatusAvailable, wantStatus: game.ItemStatusAvailable},
	} {
		t.Run(transition.name, func(t *testing.T) {
			request := testItemStatusChangePacket(t, uin, 2361, transition.newStatus, 0)
			outcome := tcpDispatchOutcome{}
			server.dispatchItemStatusStage(config, session, "hall", request, &outcome)
			if len(outcome.response) == 0 || len(outcome.beforeResponse) == 0 {
				t.Fatalf("ordered collection result = response:%d before:%d", len(outcome.response), len(outcome.beforeResponse))
			}
			if outcome.postResponse != nil {
				t.Fatal("collection transition retained a delayed refresh after producing the ordered refresh")
			}
			refreshPacket, err := game.InspectLocalPacket(outcome.beforeResponse)
			if err != nil {
				t.Fatal(err)
			}
			if refreshPacket.Command != game.PlayerItemAddNotifyCommand {
				t.Fatalf("before-response command = 0x%04X", refreshPacket.Command)
			}
			refresh, err := game.ParsePlayerItemAddNotification(refreshPacket.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(refresh.Items) != 1 || refresh.Items[0].ItemID != 2361 || refresh.Items[0].NumOfItem != 7 || refresh.Items[0].ItemStatus != transition.wantStatus {
				t.Fatalf("absolute stack refresh = %+v", refresh.Items)
			}
			ack, err := game.InspectLocalPacket(outcome.response)
			if err != nil {
				t.Fatal(err)
			}
			if ack.Command != game.ItemStatusChangeCommand || len(ack.Payload) != 2 || ack.Payload[0] != 0 || ack.Payload[1] != 0 {
				t.Fatalf("item-status ACK = 0x%04X/%x", ack.Command, ack.Payload)
			}
		})
	}
}

func testItemStatusChangePacket(t *testing.T, uin uint32, itemID uint32, status, roleID byte) []byte {
	t.Helper()
	payload := make([]byte, 16)
	binary.BigEndian.PutUint32(payload[0:4], uin)
	binary.BigEndian.PutUint32(payload[4:8], uint32(time.Now().Unix()))
	binary.BigEndian.PutUint16(payload[8:10], 1)
	binary.BigEndian.PutUint32(payload[10:14], itemID)
	payload[14], payload[15] = status, roleID
	return testLocalRoutedPacketWithPayload(t, game.ItemStatusChangeCommand, 4, 0xffff, 0, uin, payload)
}

func TestShopPurchaseRefreshesSessionAndCanBeEquipped(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Money = 3500
	profile.GameInfo.RoleID = 7
	if err := store.Save(ctx, uin, profile); err != nil {
		t.Fatal(err)
	}
	equipmentCatalog, err := equipment.NewCatalog([]itemcatalog.Entry{
		{ID: 2067, Index: 187, Name: "中山装", RegistryCategory: "cladorn"},
		{ID: 2068, Index: 188, Name: "民国学生装", RegistryCategory: "cladorn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	shopCatalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{
			{ItemID: 2067, Category: "cladorn", ResourceID: 187, Name: "中山装"},
			{ItemID: 2068, Category: "cladorn", ResourceID: 188, Name: "民国学生装"},
		},
		[]itemcatalog.CommodityEntry{
			{CommodityID: 912067, Category: "cladorn", ResourceID: 187, Name: "中山装"},
			{CommodityID: 912068, Category: "cladorn", ResourceID: 188, Name: "民国学生装"},
		},
		shopcatalog.DefaultCommodityLimit)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		playerStore: store, equipmentCatalog: equipmentCatalog, shopCatalog: shopCatalog,
		liveSessions: make(map[net.Conn]*connectionSession),
	}
	session := &connectionSession{UIN: uin, Profile: profile}
	request := game.ShopBuyRequest{
		UIN: uin, CommodityID: 912067, DealType: game.ShopDealTypePurchase,
		// The legacy BuyCommodity bridge leaves catalog revision negotiation to
		// the shop-list flow. Purchase settlement must use the authoritative
		// CommodityID lookup even when this compatibility field is stale.
		PayType: game.ShopPayTypeGameMoney, CommodityVersion: shopCatalog.Version() + 1,
	}
	response, outcome, err := settleShopPurchaseUnderConnectionLock(t, server, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.ResultID != game.ShopBuyResultSuccess || outcome != "success_item_2067_money_2500" || response.TicketLeft != 2500 {
		t.Fatalf("response/outcome = %+v / %s", response, outcome)
	}
	if len(session.Profile.Inventory) != 1 || session.Profile.Inventory[0].ItemID != 2067 {
		t.Fatalf("session inventory = %+v", session.Profile.Inventory)
	}
	if err := server.updateSessionItemStatuses(session, game.ItemStatusChangeRequest{
		UIN: uin, Items: []game.ItemStatusChange{{ItemID: 2067, NewStatus: 1, NewRoleID: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	response, _, err = settleShopPurchaseUnderConnectionLock(t, server, session, game.ShopBuyRequest{
		UIN: uin, CommodityID: 912068, DealType: game.ShopDealTypePurchase,
		PayType: game.ShopPayTypeGameMoney, AgreeMixedPayment: 1,
	})
	if err != nil || response.ResultID != game.ShopBuyResultSuccess || response.TicketLeft != 1500 {
		t.Fatalf("second purchase = %+v / %v", response, err)
	}
	if err := server.updateSessionItemStatuses(session, game.ItemStatusChangeRequest{
		UIN: uin, Items: []game.ItemStatusChange{{ItemID: 2068, NewStatus: 1, NewRoleID: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	assignments, err := store.LoadEquipment(ctx, uin)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].RoleID != 7 || assignments[0].ItemID != 2068 {
		t.Fatalf("assignments = %+v", assignments)
	}
}

func TestShopPurchaseReportsInventoryKindLimit(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Money = 1_000
	for itemID := uint16(1_000); len(profile.Inventory) < game.MaxItemInfoCount; itemID++ {
		profile.Inventory = append(profile.Inventory, game.NewPermanentItemInfo(itemID, 1))
	}
	if err = store.Save(ctx, uin, profile); err != nil {
		t.Fatal(err)
	}
	catalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{{ItemID: 20_001, Category: "item", ResourceID: 1, Name: "新道具"}},
		[]itemcatalog.CommodityEntry{{CommodityID: 90_001, Category: "item", ResourceID: 1, Name: "新道具"}},
		shopcatalog.DefaultCommodityLimit)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{playerStore: store, shopCatalog: catalog}
	response, outcome, err := settleShopPurchaseUnderConnectionLock(t, server, &connectionSession{UIN: uin, Profile: profile}, game.ShopBuyRequest{
		UIN: uin, CommodityID: 90_001, DealType: game.ShopDealTypePurchase, PayType: game.ShopPayTypeGameMoney,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ResultID != game.ShopBuyResultFailed || response.ResultString != "背包种类已满，最多 500 种" || outcome != "inventory_full" {
		t.Fatalf("inventory-full shop response = %+v / %s", response, outcome)
	}
}

func settleShopPurchaseUnderConnectionLock(t *testing.T, server *Server, session *connectionSession, request game.ShopBuyRequest) (game.ShopBuyResponse, string, error) {
	t.Helper()
	type result struct {
		response game.ShopBuyResponse
		outcome  string
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		session.mu.Lock()
		defer session.mu.Unlock()
		response, outcome, err := server.settleShopPurchaseLocked(session, request)
		completed <- result{response: response, outcome: outcome, err: err}
	}()
	select {
	case settled := <-completed:
		return settled.response, settled.outcome, settled.err
	case <-time.After(2 * time.Second):
		t.Fatal("shop purchase deadlocked while handleTCP held session.mu")
		return game.ShopBuyResponse{}, "", nil
	}
}

func TestShopPurchaseRejectsExternalCurrencyWithoutMutation(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Money = 2500
	if err := store.Save(ctx, uin, profile); err != nil {
		t.Fatal(err)
	}
	catalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{{ItemID: 2067, Category: "cladorn", ResourceID: 187}},
		[]itemcatalog.CommodityEntry{{CommodityID: 912067, Category: "cladorn", ResourceID: 187}},
		shopcatalog.DefaultCommodityLimit)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{playerStore: store, shopCatalog: catalog}
	session := &connectionSession{UIN: uin, Profile: profile}
	for _, purchase := range []struct {
		payType game.ShopPaymentMethod
		mixed   uint16
		message string
	}{
		{game.ShopPayTypeQCoin, 0, "Q币不足"},
		{game.ShopPayTypeQCoin, 1, "Q币或Q点不足"},
		{game.ShopPayTypeQPoint, 0, "Q点不足"},
		{game.ShopPayTypeQPoint, 1, "Q点或Q币不足"},
		{game.ShopPayTypeVNet, 1, "该支付方式不可用"},
		{game.ShopPayTypeKubiGem, 0, "酷比宝石不足"},
	} {
		response, outcome, err := settleShopPurchaseUnderConnectionLock(t, server, session, game.ShopBuyRequest{
			UIN: uin, CommodityID: 912067, DealType: game.ShopDealTypePurchase,
			PayType: purchase.payType, AgreeMixedPayment: purchase.mixed,
			CommodityVersion: catalog.Version() + 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.ResultID != game.ShopBuyResultFailed ||
			outcome != fmt.Sprintf("external_funds_unavailable_pay_type_%d", purchase.payType) ||
			response.ResultString != purchase.message || response.PayMoney != 0 {
			t.Fatalf("response/outcome = %+v / %s", response, outcome)
		}
	}
	reloaded, err := store.Load(ctx, uin)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GameInfo.Money != 2500 || len(reloaded.Inventory) != 0 {
		t.Fatalf("rejected purchase mutated profile = %+v", reloaded)
	}
}
