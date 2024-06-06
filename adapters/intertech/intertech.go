package intertech

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v2/adapters"
	"github.com/prebid/prebid-server/v2/config"
	"github.com/prebid/prebid-server/v2/errortypes"
	"github.com/prebid/prebid-server/v2/macros"
	"github.com/prebid/prebid-server/v2/openrtb_ext"
)

const (
	refererQueryKey  = "target-ref"
	currencyQueryKey = "ssp-cur"
	impIdQueryKey    = "imp-id"
)

// intertechPlacementID is the composite id of an ad placement
type intertechPlacementID struct {
	PageID string
	ImpID  string
}

type adapter struct {
	endpoint *template.Template
}

func Builder(bidderName openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {
	template, err := template.New("endpointTemplate").Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to parse endpoint url template: %v", err)
	}

	bidder := &adapter{
		endpoint: template,
	}

	return bidder, nil
}

func (a *adapter) MakeRequests(requestData *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var (
		requests []*adapters.RequestData
		errors   []error
	)

	referer := getReferer(requestData)
	currency := getCurrency(requestData)

	for i := range requestData.Imp {
		imp := requestData.Imp[i]

		placementId, err := getIntertechPlacementId(imp)
		if err != nil {
			errors = append(errors, err)
			continue
		}

		if err := modifyImp(&imp); err != nil {
			errors = append(errors, err)
			continue
		}

		resolvedUrl, err := a.resolveUrl(*placementId, referer, currency)
		if err != nil {
			errors = append(errors, err)
			continue
		}

		splittedRequestData := splitRequestDataByImp(requestData, imp)

		requestBody, err := json.Marshal(splittedRequestData)
		if err != nil {
			errors = append(errors, err)
			continue
		}

		requests = append(requests, &adapters.RequestData{
			Method:  "POST",
			Uri:     resolvedUrl,
			Body:    requestBody,
			Headers: getHeaders(&splittedRequestData),
			ImpIDs:  openrtb_ext.GetImpIDs(splittedRequestData.Imp),
		})
	}

	return requests, errors
}

func getHeaders(request *openrtb2.BidRequest) http.Header {
	headers := http.Header{}

	if request.Device != nil && request.Site != nil {
		addNonEmptyHeader(&headers, "Referer", request.Site.Page)
		addNonEmptyHeader(&headers, "Accept-Language", request.Device.Language)
		addNonEmptyHeader(&headers, "User-Agent", request.Device.UA)
		addNonEmptyHeader(&headers, "X-Forwarded-For", request.Device.IP)
		addNonEmptyHeader(&headers, "X-Real-Ip", request.Device.IP)
		addNonEmptyHeader(&headers, "X-Forwarded-For-IPv6", request.Device.IPv6)
		headers.Add("Content-Type", "application/json;charset=utf-8")
		headers.Add("Accept", "application/json")
	}

	return headers
}

func addNonEmptyHeader(headers *http.Header, key, value string) {
	if len(value) > 0 {
		headers.Add(key, value)
	}
}

// splitRequestDataByImp makes a shallow copy of the request for further modification (imp is already a shallow copy)
func splitRequestDataByImp(request *openrtb2.BidRequest, imp openrtb2.Imp) openrtb2.BidRequest {
	requestCopy := *request
	requestCopy.Imp = []openrtb2.Imp{imp}

	return requestCopy
}

func getIntertechPlacementId(imp openrtb2.Imp) (*intertechPlacementID, error) {
	var ext adapters.ExtImpBidder
	if err := json.Unmarshal(imp.Ext, &ext); err != nil {
		return nil, &errortypes.BadInput{
			Message: fmt.Sprintf("imp %s: unable to unmarshal ext", imp.ID),
		}
	}

	var intertechExt openrtb_ext.ExtImpIntertech
	if err := json.Unmarshal(ext.Bidder, &intertechExt); err != nil {
		return nil, &errortypes.BadInput{
			Message: fmt.Sprintf("imp %s: unable to unmarshal ext.bidder: %v", imp.ID, err),
		}
	}

	placementID, err := mapExtToPlacementID(intertechExt)
	if err != nil {
		return nil, err
	}

	return placementID, nil
}

func mapExtToPlacementID(intertechExt openrtb_ext.ExtImpIntertech) (*intertechPlacementID, error) {
	var placementID intertechPlacementID

	if len(intertechExt.PlacementID) == 0 {
		placementID.ImpID = strconv.Itoa(int(intertechExt.ImpID))
		placementID.PageID = strconv.Itoa(int(intertechExt.PageID))
		return &placementID, nil
	}

	idParts := strings.Split(intertechExt.PlacementID, "-")
	placementID.PageID = idParts[0]
	placementID.ImpID = idParts[1]

	return &placementID, nil
}

func modifyImp(imp *openrtb2.Imp) error {
	if imp.Banner != nil {
		banner, err := modifyBanner(*imp.Banner)
		if banner != nil {
			imp.Banner = banner
		}
		return err
	}

	return nil
}

func modifyBanner(banner openrtb2.Banner) (*openrtb2.Banner, error) {
	bannerCopy := banner
	format := bannerCopy.Format

	if bannerCopy.W == nil || bannerCopy.H == nil || *bannerCopy.W == 0 || *bannerCopy.H == 0 {
		if len(format) == 0 {
			return nil, &errortypes.BadInput{
				Message: "Invalid size provided for Banner",
			}
		}

		firstFormat := format[0]
		bannerCopy.H = &firstFormat.H
		bannerCopy.W = &firstFormat.W
	}

	return &bannerCopy, nil
}

// resolveUrl "un-templates" the endpoint by replacing macroses and adding the required query parameters
func (a *adapter) resolveUrl(placementID intertechPlacementID, referer string, currency string) (string, error) {
	params := macros.EndpointTemplateParams{PageID: placementID.PageID}

	endpointStr, err := macros.ResolveMacros(a.endpoint, params)
	if err != nil {
		return "", err
	}

	parsedUrl, err := url.Parse(endpointStr)
	if err != nil {
		return "", err
	}

	addNonEmptyQueryParams(parsedUrl, map[string]string{
		refererQueryKey:  referer,
		currencyQueryKey: currency,
		impIdQueryKey:    placementID.ImpID,
	})

	return parsedUrl.String(), nil
}

func addNonEmptyQueryParams(url *url.URL, queryMap map[string]string) {
	query := url.Query()
	for key, value := range queryMap {
		if len(value) > 0 {
			query.Add(key, value)
		}
	}

	url.RawQuery = query.Encode()
}

func getReferer(request *openrtb2.BidRequest) string {
	if request.Site == nil {
		return ""
	}

	return request.Site.Domain
}

func getCurrency(request *openrtb2.BidRequest) string {
	if len(request.Cur) == 0 {
		return ""
	}

	return request.Cur[0]
}

func (a *adapter) MakeBids(request *openrtb2.BidRequest, _ *adapters.RequestData, responseData *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if adapters.IsResponseStatusCodeNoContent(responseData) {
		return nil, nil
	}

	if err := adapters.CheckResponseStatusCodeForErrors(responseData); err != nil {
		return nil, []error{err}
	}

	var bidResponse openrtb2.BidResponse
	if err := json.Unmarshal(responseData.Body, &bidResponse); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("Bad server response: %d", err),
		}}
	}

	bidResponseWithCapacity := adapters.NewBidderResponseWithBidsCapacity(len(request.Imp))

	var errors []error

	impMap := map[string]*openrtb2.Imp{}
	for i := range request.Imp {
		imp := request.Imp[i]

		impMap[imp.ID] = &imp
	}

	for _, seatBid := range bidResponse.SeatBid {
		for i := range seatBid.Bid {
			bid := seatBid.Bid[i]

			imp, exists := impMap[bid.ImpID]
			if !exists {
				errors = append(errors, &errortypes.BadInput{
					Message: fmt.Sprintf("Invalid bid imp ID #%s does not match any imp IDs from the original bid request", bid.ImpID),
				})
				continue
			}

			bidType, err := getBidType(*imp)
			if err != nil {
				errors = append(errors, err)
				continue
			}

			bidResponseWithCapacity.Bids = append(bidResponseWithCapacity.Bids, &adapters.TypedBid{
				Bid:     &bid,
				BidType: bidType,
			})
		}
	}

	return bidResponseWithCapacity, errors
}

func getBidType(imp openrtb2.Imp) (openrtb_ext.BidType, error) {
	if imp.Native != nil {
		return openrtb_ext.BidTypeNative, nil
	}

	if imp.Banner != nil {
		return openrtb_ext.BidTypeBanner, nil
	}

	return "", &errortypes.BadInput{
		Message: fmt.Sprintf("Processing an invalid impression; cannot resolve impression type for imp #%s", imp.ID),
	}
}
