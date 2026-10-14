package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// defaultCertificateDays is how near its end a certificate gets before audit_system warns of it
	defaultCertificateDays = 30
	// certificateTime is how a NAS writes when a certificate is good until: in its own time zone, which the text does not say
	certificateTime = "Mon Jan _2 15:04:05 2006"
)

// systemAudit looks at the NAS itself: what it raises alerts about, its certificates, the services it is set to run, and its updates.
type systemAudit struct {
	certificateDays int
	now             time.Time
}

func (a systemAudit) run(ctx context.Context, r *registry) (*findings, error) {
	state, err := r.typed.System.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("asking whether the NAS is ready: %w", err)
	}
	alerts, err := r.typed.Alert.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the alerts: %w", err)
	}
	general, err := r.typed.SystemGeneral.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the NAS's time zone: %w", err)
	}
	certificates, err := r.typed.Certificate.Query(ctx, nil, truenas.CertificateQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the certificates: %w", err)
	}
	services, err := r.typed.Service.Query(ctx, nil, truenas.ServiceQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the services: %w", err)
	}
	update, err := r.typed.Update.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("asking about updates: %w", err)
	}

	f := &findings{}
	f.checked++
	if state.Answer != nil && *state.Answer != "READY" {
		f.add(warning, "not_ready", "system", "the NAS", "", fmt.Sprintf("the NAS is %s: until it is READY much of what it holds cannot be read, and what the other audits say may be out of date", *state.Answer))
	}

	for _, alert := range alerts.Answer {
		f.checked++
		// a disk's SMART alerts are audit_disks' to tell of
		if alert.Dismissed || strings.HasPrefix(alert.Klass, "SMART") {
			continue
		}
		switch alert.Level {
		case "EMERGENCY", "ALERT", "CRITICAL":
			f.add(critical, "alert", "alert", alert.Klass, "", "the NAS raised a critical alert: "+strings.TrimSpace(alert.Formatted))
		case "ERROR", "WARNING":
			f.add(warning, "alert", "alert", alert.Klass, "", "the NAS raised an alert: "+strings.TrimSpace(alert.Formatted))
		}
	}

	zone := time.UTC
	if general.Answer != nil {
		if named, err := time.LoadLocation(general.Answer.Timezone); err == nil {
			zone = named
		}
	}
	for i := range certificates.Answer {
		f.checked++
		a.certificate(f, &certificates.Answer[i], zone)
	}

	for _, s := range services.Answer {
		f.checked++
		if s.Enable && s.State != "RUNNING" {
			f.add(warning, "service_stopped", "service", s.Service, "", fmt.Sprintf("the %s service is set to start with the NAS, and is %s", s.Service, strings.ToLower(orNothing(s.State, "not running"))))
		}
	}

	if u := update.Answer; u != nil {
		f.checked++
		switch {
		case u.Code == truenas.UpdateStatusAnswerCodeERROR && u.Error != nil:
			f.add(notice, "update_check_failed", "system", "updates", "", "the NAS could not find out whether there is an update: "+u.Error.Reason)
		case u.Status != nil && u.Status.NewVersion != nil && u.Status.NewVersion.Version != "":
			f.add(notice, "update_available", "system", "updates", "", fmt.Sprintf("TrueNAS %s is available", u.Status.NewVersion.Version))
		}
	}

	return f, nil
}

// certificate is what is wrong with when one certificate runs out.
func (a systemAudit) certificate(f *findings, c *truenas.CertificateQueryResultItem, zone *time.Location) {
	until, err := time.ParseInLocation(certificateTime, c.Until, zone)
	if err != nil {
		return
	}

	switch left := until.Sub(a.now); {
	case left <= 0:
		f.add(critical, "certificate_expired", "certificate", c.Name, "", fmt.Sprintf("the certificate %s ran out at %s: whatever shows it is no longer trusted", c.Name, when(until)))
	case left <= time.Duration(a.certificateDays)*24*time.Hour:
		f.add(warning, "certificate_expiring", "certificate", c.Name, "", fmt.Sprintf("the certificate %s runs out in %d days, at %s", c.Name, int(left.Hours()/24), when(until)))
	}
}

func registerSystemAudits(r *registry) {
	type auditSystemIn struct {
		CertificateDays int `json:"certificate_days,omitempty" jsonschema:"how near its end a certificate gets before it is a warning, in days; 30 unless given"`
		auditIn
	}
	add(r, readTool, &mcp.Tool{
		Name:        "audit_system",
		Description: "Find what is wrong with the NAS itself: an alert it raised at warning or graver, a certificate run out or running out within 30 days, a service set to start that is not running, a NAS that is not ready, and an update waiting or a check for one that failed. A disk's SMART alerts are in audit_disks.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditSystemIn) (*mcp.CallToolResult, auditOut, error) {
		days := in.CertificateDays
		if days <= 0 {
			days = defaultCertificateDays
		}
		f, err := systemAudit{certificateDays: days, now: time.Now()}.run(ctx, r)
		if err != nil {
			return nil, auditOut{}, err
		}

		return nil, f.answer(in.pageIn), nil
	})
}
