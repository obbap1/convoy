package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/frain-dev/convoy"
	"github.com/frain-dev/convoy/config"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/net"
	"github.com/frain-dev/convoy/queue"
	"github.com/frain-dev/convoy/smtp"
	"github.com/frain-dev/convoy/util"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var ErrDeliveryAttemptFailed = errors.New("Error sending event")

type EndpointError struct {
	delay time.Duration
	Err   error
}

func (e *EndpointError) Error() string {
	return e.Err.Error()
}

func (e *EndpointError) Delay() time.Duration {
	return e.delay
}

func ProcessEventDelivery(appRepo datastore.ApplicationRepository, eventDeliveryRepo datastore.EventDeliveryRepository, groupRepo datastore.GroupRepository) func(*queue.Job) error {
	return func(job *queue.Job) error {
		Id := job.ID

		// Load message from DB and switch state to prevent concurrent processing.
		m, err := eventDeliveryRepo.FindEventDeliveryByID(context.Background(), Id)

		if err != nil {
			log.WithError(err).Errorf("Failed to load event - %s", Id)
			return nil
		}

		switch m.Status {
		case datastore.ProcessingEventStatus,
			datastore.SuccessEventStatus:
			return nil
		}

		err = eventDeliveryRepo.UpdateStatusOfEventDelivery(context.Background(), *m, datastore.ProcessingEventStatus)
		if err != nil {
			log.WithError(err).Error("failed to update status of messages - ")
			return nil
		}

		var attempt datastore.DeliveryAttempt
		var secret = m.EndpointMetadata.Secret

		cfg, err := config.Get()
		if err != nil {
			return &EndpointError{Err: err}
		}

		dispatch := net.NewDispatcher()

		var done = true

		e := m.EndpointMetadata
		if m.Status == datastore.SuccessEventStatus {
			log.Debugf("endpoint %s already merged with message %s\n", e.TargetURL, m.UID)
			return nil
		}

		dbEndpoint, err := appRepo.FindApplicationEndpointByID(context.Background(), m.AppMetadata.UID, e.UID)
		if err != nil {
			log.WithError(err).Errorf("could not retrieve endpoint %s", e.UID)
			return &EndpointError{Err: err}
		}

		if dbEndpoint.Status == datastore.InactiveEndpointStatus {
			log.Debugf("endpoint %s is inactive, failing to send.", e.TargetURL)
			return nil
		}

		buff := bytes.NewBuffer([]byte{})
		encoder := json.NewEncoder(buff)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(m.Metadata.Data); err != nil {
			log.WithError(err).Error("Failed to encode data")
			return &EndpointError{Err: err}
		}

		bStr := strings.TrimSuffix(buff.String(), "\n")

		g, err := groupRepo.FetchGroupByID(context.Background(), m.AppMetadata.GroupID)
		if err != nil {
			log.WithError(err).Error("could not find error")
			return &EndpointError{Err: err}
		}

		hmac, err := util.ComputeJSONHmac(g.Config.Signature.Hash, bStr, secret, false)
		if err != nil {
			log.Errorf("error occurred while generating hmac signature - %+v\n", err)
			return &EndpointError{Err: err}
		}

		attemptStatus := false
		start := time.Now()

		resp, err := dispatch.SendRequest(e.TargetURL, string(convoy.HttpPost), []byte(bStr), g.Config.Signature.Header.String(), hmac, int64(cfg.MaxResponseSize))
		status := "-"
		statusCode := 0
		if resp != nil {
			status = resp.Status
			statusCode = resp.StatusCode
		}

		duration := time.Since(start)
		// log request details
		requestLogger := log.WithFields(log.Fields{
			"status":   status,
			"uri":      e.TargetURL,
			"method":   convoy.HttpPost,
			"duration": duration,
		})

		if err == nil && statusCode >= 200 && statusCode <= 299 {
			requestLogger.Infof("%s", m.UID)
			log.Infof("%s sent", m.UID)
			attemptStatus = true
			e.Sent = true

			m.Status = datastore.SuccessEventStatus
			m.Description = ""
		} else {
			requestLogger.Errorf("%s", m.UID)
			done = false
			e.Sent = false

			m.Status = datastore.RetryEventStatus

			delay := m.Metadata.IntervalSeconds
			nextTime := time.Now().Add(time.Duration(delay) * time.Second)
			m.Metadata.NextSendTime = primitive.NewDateTimeFromTime(nextTime)
			attempts := m.Metadata.NumTrials + 1

			log.Errorf("%s next retry time is %s (strategy = %s, delay = %d, attempts = %d/%d)\n", m.UID, nextTime.Format(time.ANSIC), m.Metadata.Strategy, delay, attempts, m.Metadata.RetryLimit)
		}

		// Request failed but statusCode is 200 <= x <= 299
		if err != nil {
			log.Errorf("%s failed. Reason: %s", m.UID, err)
		}

		if done && dbEndpoint.Status == datastore.PendingEndpointStatus && g.Config.DisableEndpoint {
			endpoints := []string{dbEndpoint.UID}
			endpointStatus := datastore.ActiveEndpointStatus

			err := appRepo.UpdateApplicationEndpointsStatus(context.Background(), m.AppMetadata.UID, endpoints, endpointStatus)
			if err != nil {
				log.WithError(err).Error("Failed to reactivate endpoint after successful retry")
			}

			s, err := smtp.New(&cfg.SMTP)
			if err == nil {
				err = sendEmailNotification(m, g, s, endpointStatus)
				if err != nil {
					log.WithError(err).Error("Failed to send notification email")
				}
			}
		}

		if !done && dbEndpoint.Status == datastore.PendingEndpointStatus {
			endpoints := []string{dbEndpoint.UID}
			endpointStatus := datastore.InactiveEndpointStatus

			err := appRepo.UpdateApplicationEndpointsStatus(context.Background(), m.AppMetadata.UID, endpoints, endpointStatus)
			if err != nil {
				log.WithError(err).Error("Failed to reactivate endpoint after successful retry")
			}
		}

		attempt = parseAttemptFromResponse(m, e, resp, attemptStatus)

		m.Metadata.NumTrials++

		if m.Metadata.NumTrials >= m.Metadata.RetryLimit {
			if done {
				if m.Status != datastore.SuccessEventStatus {
					log.Errorln("an anomaly has occurred. retry limit exceeded, fan out is done but event status is not successful")
					m.Status = datastore.FailureEventStatus
				}
			} else {
				log.Errorf("%s retry limit exceeded ", m.UID)
				m.Description = "Retry limit exceeded"
				m.Status = datastore.FailureEventStatus
			}

			if g.Config.DisableEndpoint && dbEndpoint.Status != datastore.PendingEndpointStatus {
				endpoints := []string{dbEndpoint.UID}
				endpointStatus := datastore.InactiveEndpointStatus

				err := appRepo.UpdateApplicationEndpointsStatus(context.Background(), m.AppMetadata.UID, endpoints, endpointStatus)
				if err != nil {
					log.WithError(err).Error("Failed to reactivate endpoint after successful retry")
				}

				s, err := smtp.New(&cfg.SMTP)
				if err == nil {
					err = sendEmailNotification(m, g, s, endpointStatus)
					if err != nil {
						log.WithError(err).Error("Failed to send notification email")
					}
				}
			}
		}

		err = eventDeliveryRepo.UpdateEventDeliveryWithAttempt(context.Background(), *m, attempt)
		if err != nil {
			log.WithError(err).Error("failed to update message ", m.UID)
		}

		if !done && m.Metadata.NumTrials < m.Metadata.RetryLimit {
			delay := time.Duration(m.Metadata.IntervalSeconds) * time.Second
			return &EndpointError{Err: ErrDeliveryAttemptFailed, delay: delay}
		}

		return nil
	}
}

func sendEmailNotification(m *datastore.EventDelivery, g *datastore.Group, s *smtp.SmtpClient, status datastore.EndpointStatus) error {
	email := m.AppMetadata.SupportEmail

	logoURL := g.LogoURL

	err := s.SendEmailNotification(email, logoURL, m.EndpointMetadata.TargetURL, status)
	if err != nil {
		return err
	}

	return nil
}

func parseAttemptFromResponse(m *datastore.EventDelivery, e *datastore.EndpointMetadata, resp *net.Response, attemptStatus bool) datastore.DeliveryAttempt {

	responseHeader := util.ConvertDefaultHeaderToCustomHeader(&resp.ResponseHeader)
	requestHeader := util.ConvertDefaultHeaderToCustomHeader(&resp.RequestHeader)

	return datastore.DeliveryAttempt{
		ID:         primitive.NewObjectID(),
		UID:        uuid.New().String(),
		URL:        resp.URL.String(),
		Method:     resp.Method,
		MsgID:      m.UID,
		EndpointID: e.UID,
		APIVersion: "2021-08-27",

		IPAddress:        resp.IP,
		ResponseHeader:   *responseHeader,
		RequestHeader:    *requestHeader,
		HttpResponseCode: resp.Status,
		ResponseData:     string(resp.Body),
		Error:            resp.Error,
		Status:           attemptStatus,

		CreatedAt: primitive.NewDateTimeFromTime(time.Now()),
		UpdatedAt: primitive.NewDateTimeFromTime(time.Now()),
	}
}
