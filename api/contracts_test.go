package api

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"golang.org/x/net/context"

	"chain/api/asset"
	"chain/api/asset/assettest"
	"chain/api/smartcontracts/orderbook"
	"chain/database/pg/pgtest"
	"chain/fedchain/bc"
	"chain/net/http/httpjson"
	"chain/testutil"
)

type contractsFixtureInfo struct {
	projectID, managerNodeID, issuerNodeID, sellerAccountID string
	aaplAssetID, usdAssetID                                 bc.AssetID
	prices                                                  []*orderbook.Price
}

var ttl = time.Hour

func TestOfferContractViaBuild(t *testing.T) {
	withContractsFixture(t, func(ctx context.Context, fixtureInfo *contractsFixtureInfo) {
		buildRequest := &BuildRequest{
			Sources: []*Source{
				&Source{
					AssetID:   &fixtureInfo.aaplAssetID,
					Amount:    100,
					AccountID: fixtureInfo.sellerAccountID,
					Type:      "account",
				},
			},
			Dests: []*Destination{
				&Destination{
					AssetID:   &fixtureInfo.aaplAssetID,
					Amount:    100,
					AccountID: fixtureInfo.sellerAccountID,
					OrderbookPrices: []*orderbook.Price{
						&orderbook.Price{
							AssetID:       fixtureInfo.usdAssetID,
							OfferAmount:   1,
							PaymentAmount: 110,
						},
					},
					Type: "orderbook",
				},
			},
		}
		callBuildSingle(t, ctx, buildRequest, func(txTemplate *asset.TxTemplate) {
			err := asset.SignTxTemplate(txTemplate, testutil.TestXPrv)
			if err != nil {
				t.Fatal(err)
			}

			offerTx, err := asset.FinalizeTx(ctx, txTemplate)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}

			if len(offerTx.Outputs) != 1 {
				t.Fatalf("got %d outputs, want %d", len(offerTx.Outputs), 1)
			}

			if offerTx.Outputs[0].AssetID != fixtureInfo.aaplAssetID {
				t.Fatalf("wrong asset id. got %s, want %s", offerTx.Outputs[0].AssetID, fixtureInfo.aaplAssetID)
			}

			if offerTx.Outputs[0].Amount != 100 {
				t.Fatalf("wrong amount. got %d, want %d", offerTx.Outputs[0].Amount, 100)
			}
		})
	})
}

func callBuildSingle(t *testing.T, ctx context.Context, request *BuildRequest, continuation func(*asset.TxTemplate)) {
	result, err := buildSingle(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if dict, ok := result.(map[string]interface{}); ok {
		if template, ok := dict["template"]; ok {
			if txTemplate, ok := template.(*asset.TxTemplate); ok {
				continuation(txTemplate)
			} else {
				t.Fatal("expected result[\"template\"] to be a TxTemplate")
			}
		} else {
			t.Fatal("expected result to contain \"template\"")
		}
	} else {
		t.Fatal("expected result to be a map")
	}
}

func TestFindAndBuyContractViaBuild(t *testing.T) {
	withContractsFixture(t, func(ctx context.Context, fixtureInfo *contractsFixtureInfo) {
		openOrder, err := offerAndFind(ctx, fixtureInfo)
		if err != nil {
			t.Fatal(err)
		}

		buyerAccountID := assettest.CreateAccountFixture(ctx, t, fixtureInfo.managerNodeID, "buyer", nil)

		// Issue USD assets to buy with
		usd2200 := &bc.AssetAmount{
			AssetID: fixtureInfo.usdAssetID,
			Amount:  2200,
		}
		issueDest, err := asset.NewAccountDestination(ctx, usd2200, buyerAccountID, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		issueTxTemplate, err := asset.Issue(ctx, fixtureInfo.usdAssetID.String(), []*asset.Destination{issueDest})
		if err != nil {
			t.Fatal(err)
		}
		_, err = asset.FinalizeTx(ctx, issueTxTemplate)
		if err != nil {
			t.Fatal(err)
		}

		sellerScript, err := openOrder.SellerScript()
		if err != nil {
			t.Fatal(err)
		}

		buildRequest := &BuildRequest{
			Sources: []*Source{
				&Source{
					AssetID:   &fixtureInfo.usdAssetID,
					Amount:    2200,
					AccountID: buyerAccountID,
					Type:      "account",
				},
				&Source{
					Amount:         20, // shares of AAPL
					PaymentAssetID: &fixtureInfo.usdAssetID,
					PaymentAmount:  2200, // USD
					TxHash:         &openOrder.Hash,
					Index:          &openOrder.Index,
					Type:           "orderbook-redeem",
				},
			},
			Dests: []*Destination{
				&Destination{
					AssetID: &fixtureInfo.usdAssetID,
					Amount:  2200,
					Address: sellerScript,
					Type:    "address",
				},
				&Destination{
					AssetID:   &fixtureInfo.aaplAssetID,
					Amount:    20,
					AccountID: buyerAccountID,
					Type:      "account",
				},
			},
		}
		callBuildSingle(t, ctx, buildRequest, func(txTemplate *asset.TxTemplate) {
			err := asset.SignTxTemplate(txTemplate, testutil.TestXPrv)
			if err != nil {
				t.Fatal(err)
			}

			buyTx, err := asset.FinalizeTx(ctx, txTemplate)
			if err != nil {
				t.Fatal(err)
			}

			assettest.ExpectMatchingOutputs(t, buyTx, 1, "sending payment to seller", func(t *testing.T, txOutput *bc.TxOutput) bool {
				return reflect.DeepEqual(txOutput.Script, sellerScript)
			})
		})
	})
}

func offerAndFind(ctx context.Context, fixtureInfo *contractsFixtureInfo) (*orderbook.OpenOrder, error) {
	assetAmount := &bc.AssetAmount{
		AssetID: fixtureInfo.aaplAssetID,
		Amount:  100,
	}
	source := asset.NewAccountSource(ctx, assetAmount, fixtureInfo.sellerAccountID)
	sources := []*asset.Source{source}

	orderInfo := &orderbook.OrderInfo{
		SellerAccountID: fixtureInfo.sellerAccountID,
		Prices:          fixtureInfo.prices,
	}

	destination, err := orderbook.NewDestination(ctx, assetAmount, orderInfo, false, nil)
	if err != nil {
		return nil, err
	}
	destinations := []*asset.Destination{destination}

	offerTxTemplate, err := asset.Build(ctx, nil, sources, destinations, nil, ttl)
	if err != nil {
		return nil, err
	}
	err = asset.SignTxTemplate(offerTxTemplate, testutil.TestXPrv)
	if err != nil {
		return nil, err
	}
	_, err = asset.FinalizeTx(ctx, offerTxTemplate)
	if err != nil {
		return nil, err
	}

	req1 := globalFindOrder{
		OfferedAssetID:  fixtureInfo.aaplAssetID,
		PaymentAssetIDs: []bc.AssetID{fixtureInfo.usdAssetID},
	}

	// Need to add an http request to the context before running Find
	httpURL, err := url.Parse("http://boop.bop/v3/contracts/orderbook?status=open")
	httpReq := http.Request{URL: httpURL}
	ctx = httpjson.WithRequest(ctx, &httpReq)

	// Now find that open order
	openOrders, err := findOrders(ctx, req1)
	if err != nil {
		return nil, err
	}

	if len(openOrders) != 1 {
		return nil, fmt.Errorf("expected 1 open order, got %d", len(openOrders))
	}

	return openOrders[0], nil
}

func TestFindAndCancelContractViaBuild(t *testing.T) {
	withContractsFixture(t, func(ctx context.Context, fixtureInfo *contractsFixtureInfo) {
		openOrder, err := offerAndFind(ctx, fixtureInfo)
		if err != nil {
			t.Fatal(err)
		}
		buildRequest := &BuildRequest{
			Sources: []*Source{
				&Source{
					TxHash: &openOrder.Hash,
					Index:  &openOrder.Index,
					Type:   "orderbook-cancel",
				},
			},
			Dests: []*Destination{
				&Destination{
					AssetID:   &fixtureInfo.aaplAssetID,
					Amount:    100,
					AccountID: fixtureInfo.sellerAccountID,
					Type:      "account",
				},
			},
		}
		callBuildSingle(t, ctx, buildRequest, func(txTemplate *asset.TxTemplate) {
			err := asset.SignTxTemplate(txTemplate, testutil.TestXPrv)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}

			_, err = asset.FinalizeTx(ctx, txTemplate)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}

			// Make a block so the order should go away
			_, err = asset.MakeBlock(ctx)
			if err != nil {
				t.Fatal(err)
			}

			// Make sure that order is gone now
			found, err := orderbook.FindOpenOrderByOutpoint(ctx, &openOrder.Outpoint)
			if err != nil {
				t.Fatal(err)
			}
			if found != nil {
				t.Fatal("expected order to be gone after cancellation and block-landing")
			}
		})
	})
}

func withContractsFixture(t *testing.T, fn func(context.Context, *contractsFixtureInfo)) {
	ctx := assettest.NewContextWithGenesisBlock(t)
	defer pgtest.Finish(ctx)

	var fixtureInfo contractsFixtureInfo

	fixtureInfo.projectID = assettest.CreateProjectFixture(ctx, t, "", "")
	fixtureInfo.managerNodeID = assettest.CreateManagerNodeFixture(ctx, t, fixtureInfo.projectID, "", nil, nil)
	fixtureInfo.issuerNodeID = assettest.CreateIssuerNodeFixture(ctx, t, fixtureInfo.projectID, "", nil, nil)
	fixtureInfo.sellerAccountID = assettest.CreateAccountFixture(ctx, t, fixtureInfo.managerNodeID, "seller", nil)
	fixtureInfo.aaplAssetID = assettest.CreateAssetFixture(ctx, t, fixtureInfo.issuerNodeID, "")
	fixtureInfo.usdAssetID = assettest.CreateAssetFixture(ctx, t, fixtureInfo.issuerNodeID, "")
	fixtureInfo.prices = []*orderbook.Price{
		&orderbook.Price{
			AssetID:       fixtureInfo.usdAssetID,
			OfferAmount:   1,
			PaymentAmount: 110,
		},
	}

	aapl100 := &bc.AssetAmount{
		AssetID: fixtureInfo.aaplAssetID,
		Amount:  100,
	}
	issueDest, err := asset.NewAccountDestination(ctx, aapl100, fixtureInfo.sellerAccountID, false, nil)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	txTemplate, err := asset.Issue(ctx, fixtureInfo.aaplAssetID.String(), []*asset.Destination{issueDest})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	_, err = asset.FinalizeTx(ctx, txTemplate)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	fn(ctx, &fixtureInfo)
}