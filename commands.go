package cli53

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/miekg/dns"
)

const ChangeBatchSize = 100

func createZone(name, comment, vpcId, vpcRegion string) {
	callerReference := uniqueReference()
	req := route53.CreateHostedZoneInput{
		CallerReference: &callerReference,
		Name:            &name,
		HostedZoneConfig: &route53.HostedZoneConfig{
			Comment: &comment,
		},
	}

	if vpcId != "" && vpcRegion != "" {
		req.VPC = &route53.VPC{
			VPCId:     aws.String(vpcId),
			VPCRegion: aws.String(vpcRegion),
		}
	}
	resp, err := r53.CreateHostedZone(&req)
	fatalIfErr(err)
	fmt.Printf("Created zone: '%s' ID: '%s'\n", *resp.HostedZone.Name, *resp.HostedZone.Id)
}

func purgeZoneRecords(id string, wait bool) {
	rrsets, err := ListAllRecordSets(r53, id)
	fatalIfErr(err)

	// delete all non-default SOA/NS records
	changes := []*route53.Change{}
	for _, rrset := range rrsets {
		if *rrset.Type != "NS" && *rrset.Type != "SOA" {
			change := &route53.Change{
				Action:            aws.String("DELETE"),
				ResourceRecordSet: rrset,
			}
			changes = append(changes, change)
		}
	}

	if len(changes) > 0 {
		req2 := route53.ChangeResourceRecordSetsInput{
			HostedZoneId: &id,
			ChangeBatch: &route53.ChangeBatch{
				Changes: changes,
			},
		}
		resp, err := r53.ChangeResourceRecordSets(&req2)
		fatalIfErr(err)
		fmt.Printf("%d record sets deleted\n", len(changes))
		if wait {
			waitForChange(resp.ChangeInfo)
		}
	}
}

func deleteZone(name string, purge bool) {
	zone := lookupZone(name)
	if purge {
		purgeZoneRecords(*zone.Id, false)
	}
	req := route53.DeleteHostedZoneInput{Id: zone.Id}
	_, err := r53.DeleteHostedZone(&req)
	fatalIfErr(err)
	fmt.Printf("Deleted zone: '%s' ID: '%s'\n", *zone.Name, *zone.Id)
}

func listZones() {
	req := route53.ListHostedZonesInput{}
	for {
		// paginated
		resp, err := r53.ListHostedZones(&req)
		fatalIfErr(err)
		for _, zone := range resp.HostedZones {
			fmt.Printf("%+v\n", zone)
		}
		if *resp.IsTruncated {
			req.Marker = resp.NextMarker
		} else {
			break
		}
	}
}

func isAuthRecord(zone *route53.HostedZone, rrset *route53.ResourceRecordSet) bool {
	return (*rrset.Type == "SOA" || *rrset.Type == "NS") && *rrset.Name == *zone.Name
}

func expandSelfAliases(records []dns.RR, zone *route53.HostedZone) {
	for _, record := range records {
		expandSelfAlias(record, zone)
	}
}

func expandSelfAlias(record dns.RR, zone *route53.HostedZone) {
	if alias, ok := record.(*dns.PrivateRR); ok {
		rdata := alias.Data.(*ALIAS)
		if rdata.ZoneId == "$self" {
			rdata.ZoneId = strings.Replace(*zone.Id, "/hostedzone/", "", 1)
			rdata.Target = qualifyName(rdata.Target, *zone.Name)
		}
	}
}

type Key struct {
	Name       string
	Rrtype     uint16
	Identifier string
}

type changeSorter struct {
	changes []*route53.Change
}

func (r changeSorter) Len() int {
	return len(r.changes)
}

func (r changeSorter) Swap(i, j int) {
	r.changes[i], r.changes[j] = r.changes[j], r.changes[i]
}

func (r changeSorter) Less(i, j int) bool {
	// sort non-aliases first
	if r.changes[i].ResourceRecordSet.AliasTarget == nil {
		return true
	}
	if r.changes[j].ResourceRecordSet.AliasTarget == nil {
		return false
	}
	return *r.changes[i].ResourceRecordSet.Name < *r.changes[j].ResourceRecordSet.Name
}

func groupRecords(records []dns.RR) map[Key][]dns.RR {
	// group records by name+type and optionally identifier
	grouped := map[Key][]dns.RR{}
	for _, record := range records {
		var identifier string
		if aws, ok := record.(*AWSRR); ok {
			identifier = aws.Identifier
		}
		key := Key{record.Header().Name, record.Header().Rrtype, identifier}
		grouped[key] = append(grouped[key], record)
	}
	return grouped
}

func importBind(name string, file string, wait bool, editauth bool, replace bool) {
	zone := lookupZone(name)
	records := parseBindFile(file, *zone.Name)
	expandSelfAliases(records, zone)

	grouped := groupRecords(records)
	existing := map[string]*route53.ResourceRecordSet{}
	if replace {
		rrsets, err := ListAllRecordSets(r53, *zone.Id)
		fatalIfErr(err)
		for _, rrset := range rrsets {
			if editauth || !isAuthRecord(zone, rrset) {
				rrset.Name = aws.String(unescaper.Replace(*rrset.Name))
				existing[rrset.String()] = rrset
			}
		}
	}

	additions := []*route53.Change{}
	for _, values := range grouped {
		rrset := ConvertBindToRRSet(values)
		if rrset != nil && (editauth || !isAuthRecord(zone, rrset)) {
			key := rrset.String()
			if _, ok := existing[key]; ok {
				// no difference - leave it untouched
				delete(existing, key)
			} else {
				// new record, add
				change := route53.Change{
					Action:            aws.String("CREATE"),
					ResourceRecordSet: rrset,
				}
				additions = append(additions, &change)
			}
		}
	}

	// remaining records in existing should be deleted
	deletions := []*route53.Change{}
	for _, rrset := range existing {
		change := route53.Change{
			Action:            aws.String("DELETE"),
			ResourceRecordSet: rrset,
		}
		deletions = append(deletions, &change)
	}

	resp := batchChanges(additions, deletions, zone)
	fmt.Printf("%d records imported (%d changes / %d additions / %d deletions)\n", len(records), len(additions)+len(deletions), len(additions), len(deletions))

	if wait && resp != nil {
		waitForChange(resp.ChangeInfo)
	}
}

func batchChanges(additions, deletions []*route53.Change, zone *route53.HostedZone) *route53.ChangeResourceRecordSetsOutput {
	// sort additions so aliases are last
	sort.Sort(changeSorter{additions})

	changes := append(deletions, additions...)

	var resp *route53.ChangeResourceRecordSetsOutput
	for i := 0; i < len(changes); i += ChangeBatchSize {
		end := i + ChangeBatchSize
		if end > len(changes) {
			end = len(changes)
		}
		batch := route53.ChangeBatch{
			Changes: changes[i:end],
		}
		req := route53.ChangeResourceRecordSetsInput{
			HostedZoneId: zone.Id,
			ChangeBatch:  &batch,
		}
		var err error
		resp, err = r53.ChangeResourceRecordSets(&req)
		fatalIfErr(err)
	}
	return resp
}

func UnexpandSelfAliases(records []dns.RR, zone *route53.HostedZone, full bool) {
	id := strings.Replace(*zone.Id, "/hostedzone/", "", 1)
	for _, rr := range records {
		if alias, ok := rr.(*dns.PrivateRR); ok {
			rdata := alias.Data.(*ALIAS)
			if rdata.ZoneId == id {
				rdata.ZoneId = "$self"
				if !full {
					rdata.Target = shortenName(rdata.Target, *zone.Name)
				}
			}
		}
	}
}

func exportBind(name string, full bool) {
	zone := lookupZone(name)
	ExportBindToWriter(r53, zone, full, os.Stdout)
}

type exportSorter struct {
	rrsets []*route53.ResourceRecordSet
	zone   string
}

func (r exportSorter) Len() int {
	return len(r.rrsets)
}

func (r exportSorter) Swap(i, j int) {
	r.rrsets[i], r.rrsets[j] = r.rrsets[j], r.rrsets[i]
}

func (r exportSorter) Less(i, j int) bool {
	if *r.rrsets[i].Name == *r.rrsets[j].Name {
		if *r.rrsets[i].Type == "SOA" {
			return true
		}
		return *r.rrsets[i].Type < *r.rrsets[j].Type
	}
	if *r.rrsets[i].Name == r.zone {
		return true
	}
	if *r.rrsets[j].Name == r.zone {
		return false
	}
	return *r.rrsets[i].Name < *r.rrsets[j].Name
}

func ExportBindToWriter(r53 *route53.Route53, zone *route53.HostedZone, full bool, out io.Writer) {
	rrsets, err := ListAllRecordSets(r53, *zone.Id)
	fatalIfErr(err)

	sort.Sort(exportSorter{rrsets, *zone.Name})
	dnsname := *zone.Name
	fmt.Fprintf(out, "$ORIGIN %s\n", dnsname)
	for _, rrset := range rrsets {
		rrs := ConvertRRSetToBind(rrset)
		UnexpandSelfAliases(rrs, zone, full)
		for _, rr := range rrs {
			line := rr.String()
			if !full {
				parts := strings.Split(line, "\t")
				parts[0] = shortenName(parts[0], dnsname)
				if parts[3] == "CNAME" {
					parts[4] = shortenName(parts[4], dnsname)
				}
				line = strings.Join(parts, "\t")
			}
			fmt.Fprintln(out, line)
		}
	}
}

type createArgs struct {
	name          string
	records       []string
	wait          bool
	replace       bool
	identifier    string
	failover      string
	healthCheckId string
	weight        *int
	region        string
	countryCode   string
	continentCode string
}

func (args createArgs) validate() bool {
	if args.failover != "" && args.failover != "PRIMARY" && args.failover != "SECONDARY" {
		fmt.Println("failover must be PRIMARY or SECONDARY")
		return false
	}
	extcount := 0
	if args.failover != "" {
		extcount += 1
	}
	if args.weight != nil {
		extcount += 1
	}
	if args.region != "" {
		extcount += 1
	}
	if args.countryCode != "" {
		extcount += 1
	}
	if args.continentCode != "" {
		extcount += 1
	}
	if extcount > 0 && args.identifier == "" {
		fmt.Println("identifier must be set when creating an extended record")
		return false
	}
	if extcount == 0 && args.identifier != "" {
		fmt.Println("identifier should only be set when creating an extended record")
		return false
	}
	if extcount > 1 {
		fmt.Println("failover, weight, region, country-code and continent-code are mutually exclusive")
		return false
	}
	return true
}

func (args createArgs) applyRRSetParams(rrset *route53.ResourceRecordSet) {
	if args.identifier != "" {
		rrset.SetIdentifier = aws.String(args.identifier)
	}
	if args.failover != "" {
		rrset.Failover = aws.String(args.failover)
	}
	if args.healthCheckId != "" {
		rrset.HealthCheckId = aws.String(args.healthCheckId)
	}
	if args.weight != nil {
		rrset.Weight = aws.Int64(int64(*args.weight))
	}
	if args.region != "" {
		rrset.Region = aws.String(args.region)
	}
	if args.countryCode != "" {
		rrset.GeoLocation = &route53.GeoLocation{
			CountryCode: aws.String(args.countryCode),
		}
	}
	if args.continentCode != "" {
		rrset.GeoLocation = &route53.GeoLocation{
			ContinentCode: aws.String(args.continentCode),
		}
	}
}

func equalStringPtrs(a, b *string) bool {
	if a == nil && b == nil {
		return true
	} else if a != nil && b != nil {
		return *a == *b
	} else {
		return false
	}
}

func parseRecordList(args []string, zone *route53.HostedZone) []dns.RR {
	records := []dns.RR{}
	origin := fmt.Sprintf("$ORIGIN %s\n", *zone.Name)
	for _, text := range args {
		record, err := dns.NewRR(origin + text)
		fatalIfErr(err)
		records = append(records, record)
	}
	return records
}

func createRecords(args createArgs) {
	zone := lookupZone(args.name)
	records := parseRecordList(args.records, zone)
	expandSelfAliases(records, zone)

	grouped := groupRecords(records)

	var existing []*route53.ResourceRecordSet
	if args.replace {
		var err error
		existing, err = ListAllRecordSets(r53, *zone.Id)
		fatalIfErr(err)
	}

	additions := []*route53.Change{}
	deletions := []*route53.Change{}
	for _, values := range grouped {
		rrset := ConvertBindToRRSet(values)
		args.applyRRSetParams(rrset)

		change := &route53.Change{
			Action:            aws.String("CREATE"),
			ResourceRecordSet: rrset,
		}
		additions = append(additions, change)

		if args.replace {
			// add DELETE if there is an existing record
			for _, candidate := range existing {
				if equalStringPtrs(rrset.Name, candidate.Name) &&
					equalStringPtrs(rrset.Type, candidate.Type) &&
					equalStringPtrs(rrset.SetIdentifier, candidate.SetIdentifier) {
					change := route53.Change{
						Action:            aws.String("DELETE"),
						ResourceRecordSet: candidate,
					}
					deletions = append(deletions, &change)
					break
				}
			}
		}
	}

	resp := batchChanges(additions, deletions, zone)

	for _, record := range records {
		txt := strings.Replace(record.String(), "\t", " ", -1)
		fmt.Printf("Created record: '%s'\n", txt)
	}

	if args.wait {
		waitForChange(resp.ChangeInfo)
	}
}

// Paginate request to get all record sets.
func ListAllRecordSets(r53 *route53.Route53, id string) (rrsets []*route53.ResourceRecordSet, err error) {
	req := route53.ListResourceRecordSetsInput{
		HostedZoneId: &id,
	}

	for {
		var resp *route53.ListResourceRecordSetsOutput
		resp, err = r53.ListResourceRecordSets(&req)
		if err != nil {
			return
		} else {
			rrsets = append(rrsets, resp.ResourceRecordSets...)
			if *resp.IsTruncated {
				req.StartRecordName = resp.NextRecordName
				req.StartRecordType = resp.NextRecordType
				req.StartRecordIdentifier = resp.NextRecordIdentifier
			} else {
				break
			}
		}
	}
	return
}

func deleteRecord(name string, match string, rtype string, wait bool, identifier string) {
	zone := lookupZone(name)
	rrsets, err := ListAllRecordSets(r53, *zone.Id)
	fatalIfErr(err)

	match = qualifyName(match, *zone.Name)
	changes := []*route53.Change{}
	for _, rrset := range rrsets {
		if *rrset.Name == match && *rrset.Type == rtype && (identifier == "" || *rrset.SetIdentifier == identifier) {
			change := &route53.Change{
				Action:            aws.String("DELETE"),
				ResourceRecordSet: rrset,
			}
			changes = append(changes, change)
		}
	}

	if len(changes) > 0 {
		req2 := route53.ChangeResourceRecordSetsInput{
			HostedZoneId: zone.Id,
			ChangeBatch: &route53.ChangeBatch{
				Changes: changes,
			},
		}
		resp, err := r53.ChangeResourceRecordSets(&req2)
		fatalIfErr(err)
		fmt.Printf("%d record sets deleted\n", len(changes))
		if wait {
			waitForChange(resp.ChangeInfo)
		}
	} else {
		fmt.Println("Warning: no records matched - nothing deleted")
	}
}

func purgeRecords(name string, wait bool) {
	zone := lookupZone(name)
	purgeZoneRecords(*zone.Id, wait)
}