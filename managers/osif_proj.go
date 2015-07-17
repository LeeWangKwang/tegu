// vi: sw=4 ts=4:

/*
	Mnemonic:	osif_proj.go
	Abstract:	Functions that manage an osif project struct. For now it manages the
				related translation maps for the project. In future it might also
				be used to reference the associated creds, but not wanting to change
				the structure that builds that aspect of thigs.

	Date:		17 November 2014
	Author:		E. Scott Daniels

	Mods:		16 Dec 2014 - Corrected slice out of bounds error in get_host_info()
				09 Jan 2015 - No longer assume that the gateway list is limited by the project
					that is valid in the creds.  At least some versions of Openstack were
					throwing all gateways into the subnet list.
				10 Jan 2015 - Added support to save subnet info for wa.
				16 Jan 2014 : Support port masks in flow-mods.
				26 Feb 2014 - Added support to dig out the default gateway for a project.
				31 Mar 2015 - Changes to provide a force load of all VMs into the network graph.
				01 Apr 2015 - Added ipv6 support for finding gateway/routers.
				16 Jun 2015 - Turned down some of the bleat messages.
				17 Jul 2015 - Merged steering changes in.
*/

package managers
// should this move to gizmos?

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	//"codecloud.web.att.com/gopkgs/clike"
	"codecloud.web.att.com/gopkgs/ipc"
	"codecloud.web.att.com/gopkgs/ostack"
)


/*
	All of the maps associated with a given project.
*/
type osif_project struct {
	name		*string
	lastfetch	int64						// timestamp of last map update to detect freshness
	vmid2ip		map[string]*string			// translation maps for the project
	ip2vmid		map[string]*string
	ip2vm		map[string]*string
	vm2ip		map[string]*string			// vm name to ip; gateway IPs are used as names
	vmid2host	map[string]*string
	ip2mac		map[string]*string
	gwmap		map[string]*string			// mac to ip translation
	ip2fip		map[string]*string
	fip2ip		map[string]*string
	gw2cidr		map[string]*string
	gwip2phost	map[string]*string			// gateway ten/ip to physical host map
	gwid2phost	map[string]*string			// gateway uuid to physical host map
	sninfo		map[string]*string			// subnet info indexed by subnet id

	rwlock		sync.RWMutex				// must lock to prevent update collisions
}

// -------------------------------------------------------------------------------------------------

/*
	Accepts an IP address, a network and number of bits that specify the network portion of an 
	address. Using the number of bits the IP address is converted to it's network address and 
	compared to the target network. If they match, the IP address is a member of the subnet
	and true is returned.  Works for both ip4 and ip6 addresses. Errors returned by ParseCIDR
	are ignored as they are most likely ip6 number of bits that are too large for an ip4 
	address.  This can happen when both IP address types are in use on the same cluster.
*/
func in_subnet( ip string, target_net string, nbits string ) ( bool ) {
	_, ip_net, err := net.ParseCIDR( ip + "/" + nbits )	
	if err != nil {									// this will happen if we are validating an ip4 with an ip6 and the nbits is too large; ignore
		return false
	}

	return  target_net == ip_net.IP.String() 
}


// -------------------------------------------------------------------------------------------------

/*
	Make a new project map management block.
*/
func Mk_osif_project( name string ) ( p *osif_project, err error ) {
	p = &osif_project {
		name:	&name,
		lastfetch:	0,
	}

	p.vmid2ip = make( map[string]*string )
	p.ip2vmid = make( map[string]*string )
	p.vm2ip = make( map[string]*string )
	p.ip2vm = make( map[string]*string )
	p.vmid2host = make( map[string]*string )
	p.ip2mac = make( map[string]*string )
	p.gwmap = make( map[string]*string )
	p.ip2fip = make( map[string]*string )
	p.fip2ip = make( map[string]*string )

	return
}


/*
	Run the os creds in the creds list and add any projects to the 
	project list that aren't already there. pname2id is a map that
	translates project names to a uuid.
*/
func add2projects( projects map[string]*osif_project, creds map[string]*ostack.Ostack, pname2id map[string]*string, bleat_level uint ) {

	for k, _ := range creds {					// build the projects for maps
		if k != "_ref_" {						// we don't do this for the reference project
			pid, ok := pname2id[k]					// projects are tracked with uuid
			if ok {
				if _, ok = projects[*pid]; !ok {	// project hasn't been added to the list yet
					np, err := Mk_osif_project( k )
					if err == nil {
						projects[*pname2id[k]] = np	
						osif_sheep.Baa( 1, "successfully created osif_project for: %s/%s", k, *pname2id[k] )
					} else {
						osif_sheep.Baa( 1, "unable to create  an osif_project for: %s/%s", k, *pname2id[k] )
					}
				}
			} else {
				osif_sheep.Baa( bleat_level, "project did not map to an id: %s", k )		// probably bleat on the first go
			}
		}
	}
}

/*
	Build all translation maps for the given project.
	Does NOT replace a map with a nil map; we assume this is an openstack glitch.

	CAUTION:  ip2 maps are complete, where vm2 or vmid2 maps are not because
			they only reference one of the VMs IP addresses where there might
			be many.
*/
func (p *osif_project) refresh_maps( creds *ostack.Ostack ) ( rerr error ) {
	
	if p == nil {
		return
	}
	if creds == nil {
		osif_sheep.Baa( 1, "IER: refresh_maps given nil creds" )
		rerr = fmt.Errorf( "creds were nil" )
		return
	}

	if *p.name != "_ref_" {				// we don't fetch maps from the ref since it's not real
		olastfetch := p.lastfetch		// last fetch -- ensure it wasn't fetched while we waited
		p.rwlock.Lock()					// wait for a write lock
		defer p.rwlock.Unlock()				// ensure unlocked on return

		if olastfetch != p.lastfetch {	// assume read done while we waited
			return
		}

		osif_sheep.Baa( 2, "refresh: creating VM maps from: %s", creds.To_str( ) )
		vmid2ip, ip2vmid, vm2ip, vmid2host, vmip2vm, err := creds.Mk_vm_maps( nil, nil, nil, nil, nil, true )
		if err != nil {
			osif_sheep.Baa( 2, "WRN: unable to map VM info (vm): %s; %s   [TGUOSI003]", creds.To_str( ), err )
			rerr = err
			creds.Expire()					// force re-auth next go round
		} else {

			osif_sheep.Baa( 2, "%s map sizes: vmid2ip=%d ip2vmid=%d vm2ip=%d vmid2host=%d vmip2vm=%d", 
					*p.name, len( vmid2ip ), len( ip2vmid ), len( vm2ip ), len( vmid2host ), len( vmip2vm ) )
			if len( vmip2vm ) > 0 && len( vmid2ip ) > 0 &&  len( ip2vmid ) > 0 &&  len( vm2ip ) > 0 &&  len( vmid2host ) > 0  {		// don't refresh unless all are good
				p.vmid2ip = vmid2ip						// id and vm name map to just ONE ip address
				p.vm2ip = vm2ip
				p.ip2vmid = ip2vmid						// the only complete list of ips
				p.vmid2host = vmid2host					// id to physical host
				p.ip2vm = vmip2vm
			}
		}

		fip2ip, ip2fip, err := creds.Mk_fip_maps( nil, nil, true )
		if err != nil {
			osif_sheep.Baa( 2, "WRN: unable to map VM info (fip): %s; %s   [TGUOSI004]", creds.To_str( ), err )
			rerr = err
			creds.Expire()					// force re-auth next go round
		} else {
			osif_sheep.Baa( 2, "%s map sizes: ip2fip=%d fip2ip=%d", *p.name, len( ip2fip ), len( fip2ip ) )
			if len( ip2fip ) > 0 &&  len( fip2ip ) > 0 {
				p.ip2fip = ip2fip
				p.ip2fip = fip2ip
			}
		}

		ip2mac, _, err := creds.Mk_mac_maps( nil, nil, true )	
		if err != nil {
			osif_sheep.Baa( 2, "WRN: unable to map MAC info: %s; %s   [TGUOSI005]", creds.To_str( ), err )
			rerr = err
			creds.Expire()					// force re-auth next go round
		} else {
			osif_sheep.Baa( 2, "%s map sizes: ip2mac=%d", *p.name, len( ip2mac ) )
			if len( ip2mac ) > 0  {
				p.ip2mac = ip2mac
			}
		}
	

///wa		gwmap, _, _, _, gwid2phost, gwip2phost, err := creds.Mk_gwmaps( nil, nil, nil, nil, nil, nil, true, false )		
		gwmap, _, gwmac2id, _, gwid2phost, gwip2phost, err := creds.Mk_gwmaps( nil, nil, nil, nil, nil, nil, true, false )		// gwmap is mac2ip
		if err != nil {
			osif_sheep.Baa( 2, "WRN: unable to map gateway info: %s; %s   [TGUOSI006]", creds.To_str( ), err )
			creds.Expire()					// force re-auth next go round
		} else {
			osif_sheep.Baa( 2, "%s map sizes: gwmap=%d gwip2phost=%d", *p.name, len( gwmap ), len( gwip2phost ) )
			if len( gwmap ) > 0 {
				p.gwmap = gwmap
				p.gwip2phost = gwip2phost
				p.gwid2phost = gwid2phost
			}

			for mac, id := range gwmac2id {			// run the gateway info and insert as though they were first class VMs
				ip := gwmap[mac]
				p.vmid2ip[*id] = ip
				p.ip2vmid[*ip] = id
				p.vmid2host[*id] = gwip2phost[*ip]
				p.vm2ip[*ip] = ip				// gw is nameless, so use the ip address
			}
		}

		sninfo, gw2cidr, err := creds.Mk_snlists( ) 		// get list of gateways and their subnet cidr
		if err == nil && gw2cidr != nil  && sninfo != nil {
			p.gw2cidr = gw2cidr
			p.sninfo = sninfo
		} else {
			if err != nil {
				osif_sheep.Baa( 1, "WRN: unable to create gateway to cidr map: %s; %s   [TGUOSI007]", creds.To_str( ), err )
			} else {
				osif_sheep.Baa( 1, "WRN: unable to create gateway to cidr map: %s  no reason given   [TGUOSI007]", creds.To_str( ) )
			}
			creds.Expire()					// force re-auth next go round
		}

		p.lastfetch = time.Now().Unix()
	}

	return
}

/* Suss out the gateway from the list based on the VM's ip address.
	Must look at the project on the gateway as some flavours of openstack
	seem to return every subnet, not just the subnets defined for the
	project listed in the creds.
*/
func (p *osif_project) ip2gw( ip *string ) ( *string ) {
	if p == nil || ip == nil {
		return nil
	}

	ip_toks := strings.Split( *ip, "/" )			// assume project/ip
	project := ""
	if len( ip_toks ) > 1 {						// should always be 2, but don't core dump if not
		ip = &ip_toks[1]
		project = ip_toks[0]					// capture the project for match against the gateway
	} else {
		ip = &ip_toks[0]
	}
		
	for k, v := range p.gw2cidr {												// key is the project/ip of the gate, value is the cidr
		k_toks := strings.Split( k, "/" )										// need to match on project too
		if len( k_toks ) == 1  ||  k_toks[0] ==  project || project == "" {		// safe to check the cidr
			c_toks := strings.Split( *v, "/" )
			if in_subnet( *ip, c_toks[0], c_toks[1]  ) {
				osif_sheep.Baa( 2, "mapped ip to gateway for: %s  %s", *ip, k )
				return &k
			}
		}
	}

	osif_sheep.Baa( 2, "osif-ip2gw: unable to map ip to gateway for: %s", *ip )
	return nil
}

/* Suss out the first gateway (router) for the project. Needed for E* steering case.
	Assume input (proj_stuff) is either project, project/, or project/<stuff>.
*/
func (p *osif_project) suss_default_gw( proj_stuff *string ) ( *string ) {
	if p == nil || proj_stuff == nil {
		return nil
	}

	proj_toks := strings.Split( *proj_stuff, "/" )			// could be project/<stuff>; ditch stuff
	project := proj_toks[0]
		
	for k, _ := range p.gw2cidr {												// key is the project/ip of the gate, value is the cidr
		k_toks := strings.Split( k, "/" )										// need to match on project too
		if len( k_toks ) == 1  ||  k_toks[0] ==  project || project == "" {		// found the first, return it
			osif_sheep.Baa( 2, "found default gateway for: %s  %s", project, k )
			return &k
		}
	}

	osif_sheep.Baa( 1, "osif-ip2gw: unable to find default gateway for: %s", project )

	return nil
}

/* Suss out the CIDR that the VM's IP belongs to.  We find the gateway and
	then use the gateway's ip to get the CIDR>
*/
func (p *osif_project) ip2cidr( ip4 *string ) ( *string ) {
	if p == nil || ip4 == nil {
		return nil
	}

	ip_toks := strings.Split( *ip4, "/" )			// assume project/ip
	ip := ""
	project := ""
	if len( ip_toks ) > 1 {						// should always be 2, but don't core dump if not
		ip = ip_toks[1]
		project = ip_toks[0]					// capture the project for match against the gateway
	} else {
		ip = ip_toks[0]
	}
		
	for k, v := range p.gw2cidr {												// key is the project/ip of the gate, value is the cidr
		k_toks := strings.Split( k, "/" )										// need to match on project too
		if len( k_toks ) == 1  ||  k_toks[0] ==  project || project == "" {		// safe to check the cidr
			c_toks := strings.Split( *v, "/" )
			//if in_subnet( ip, c_toks[0], clike.Atoi( c_toks[1] ) ) {
			if in_subnet( ip, c_toks[0],  c_toks[1] ) {
				osif_sheep.Baa( 1, "mapped ip to CIDR for: %s  %s", *ip4, *v )
				return v
			}
		}
	}

	osif_sheep.Baa( 1, "osif-ip2gw: unable to map ip to CIDR for: %s", *ip4 )
	return nil
}

/*
	Supports Get_info by searching for the information but does not do a reload.
*/
func (p *osif_project) suss_info( search *string ) ( name, id, ip4, fip4, mac, gw, cidr, phost *string, gwmap map[string]*string ) {

	name = nil
	id = nil
	ip4 = nil

	if p == nil || search == nil {
		return
	}

	dup_str := *search							// new string in case we need to pass it back as a return value
	search = &dup_str

	p.rwlock.RLock()							// lock for reading
	defer p.rwlock.RUnlock() 					// ensure unlocked on return

	if p.vm2ip[*search] != nil {				// search is the name
		ip4 = p.vm2ip[*search]
		name = search
	} else {
		if p.ip2vmid[*search] != nil {			// name is actually an ip
			ip4 = search
			id = p.ip2vmid[*ip4]
			name = p.ip2vm[*ip4]
		} else {								// assume its an id or project/id
			if p.vmid2ip[*search] != nil {		// id2ip shouldn't include project, but handle that case
				id = search
				ip4 = p.vmid2ip[*id]
				name = p.ip2vm[*ip4]
			} else {
				tokens := strings.Split( *search, "/" )			// could be id or project/id
				id = &tokens[0]									// assume it's just the id and not project/id
				if len( tokens ) > 1  {
					id = &tokens[1]
				}
				if p.vmid2ip[*id] != nil {
					ip4 = p.vmid2ip[*id]
					name = p.ip2vm[*ip4]
				}
			}
		}
	}

	if name == nil || ip4 == nil {
		return
	}

	if id == nil {
		id = p.ip2vmid[*ip4]
	}

	fip4 = p.ip2fip[*ip4]
	gw = p.ip2gw( ip4 )					// find the gateway for the VM
	cidr = p.ip2cidr( ip4 )
	mac = p.ip2mac[*ip4]
	phost = p.vmid2host[*id]
	gwmap = make( map[string]*string, len( p.gwmap ) )
	for k, v := range p.gwmap {
		gwmap[k] = v					// should be safe to reference the same string
	}

	return
}


/*
	Looks for the search string treating it first as a VM name, then VM IP address
	and finally VM ID (might want to redo that order some day) and if a match in
	the maps is found, we return the gambit of information.  If not found, we force
	a reload of the map and then search again.  The new-data flag indicates that the
	information wasn't in the previous map.
*/
func (p *osif_project) Get_info( search *string, creds *ostack.Ostack, inc_project bool ) (
		name *string,
		id *string,
		ip4 *string,
		fip4 *string,
		mac *string,
		gw *string,
		cidr *string, 
		phost *string,
		gwmap map[string]*string,
		new_data bool,
		err error ) {

	new_data = false
	err = nil
	name = nil
	id = nil

	if creds == nil {
		err = fmt.Errorf( "creds were nil" )
		osif_sheep.Baa( 2, "lazy update: unable to get nil creds" )
		return
	}

	if time.Now().Unix() - p.lastfetch < 90 {					// if fresh, try to avoid reload
		name, id, ip4, fip4, mac, gw, cidr, phost, gwmap = p.suss_info( search )
	}

	if name == nil {											// not found or not fresh, force reload
		osif_sheep.Baa( 2, "lazy update: data reload for: %s", *p.name )
		new_data = true		
		err = p.refresh_maps( creds )
		if err == nil {
			name, id, ip4, fip4, mac, gw, cidr, phost, gwmap = p.suss_info( search )
		}
	}

	return
}

/*	Accepts a subnet uuid and openstack creds, then attempts to find the matching 
	information in the table.  If the data is fresh, and we find the uuid in the 
	data, we'll return that info, otherwise we'll attempt a reload and return what we 
	find or error.
*/
func (p *osif_project) get_sninfo( uuid *string, creds *ostack.Ostack ) (
		name *string,
		project *string,
		cidr *string,
		ip *string,
		phost *string,
		err error ) {

	err = nil
	name = nil
	ip = nil
	cidr = nil
	phost = nil

	if creds == nil {
		err = fmt.Errorf( "creds were nil" )
		osif_sheep.Baa( 2, "lazy update: subnet info unable received nil creds" )
		return
	}

	if time.Now().Unix() - p.lastfetch < 90 {					// if fresh, try to avoid reload
		sn_str := p.sninfo[*uuid]
		if sn_str != nil {
			sn_toks := strings.Split( *sn_str, " " )			// toks are: name, project_id, cidr, router ip

			if len( sn_toks ) >= 4 {
				name = &sn_toks[0]
				project = &sn_toks[1]
				cidr = &sn_toks[2]
				pip := sn_toks[1] + "/" + sn_toks[3]		// bang projet id and ip together
				ip = &pip
				phost = p.gwip2phost[*ip]
				err = nil
			}
		}
	}

	if name == nil {											// not found or not fresh, force reload
		osif_sheep.Baa( 2, "lazy update: subnet data reload for: %s", *p.name )
		err = p.refresh_maps( creds )
		if err == nil {
			sn_str := p.sninfo[*uuid]
			if sn_str != nil {
				sn_toks := strings.Split( *sn_str, " " )		// toks are: name, project_id, cidr, router ip
				if len( sn_toks ) >= 4 {
					name = &sn_toks[0]
					project = &sn_toks[1]
					cidr = &sn_toks[2]
					pip := sn_toks[1] + "/" + sn_toks[3]		// bang projet id and ip together
					ip = &pip
					phost = p.gwip2phost[*ip]					// phost can only be related to the gw ip addr
					err = nil
				}
			}
		}
	}

	return
}

/*
	Accepts project and gateway ids and returns the physical host that the router lives on.
*/
func (p *osif_project) gw2phost( uuid *string, creds *ostack.Ostack ) ( phost *string, err error ) {

	err = nil
	phost = nil

	if creds == nil {
		err = fmt.Errorf( "creds were nil" )
		osif_sheep.Baa( 2, "lazy update/ gw2phost: received nil creds" )
		return
	}

	if time.Now().Unix() - p.lastfetch < 90 {					// if fresh, try to avoid reload
		phost = p.gwid2phost[*uuid]
	}

	if phost == nil {											// not found or not fresh, force reload
		osif_sheep.Baa( 2, "lazy update/gw2phost: data reload for: %s", *p.name )
		err = p.refresh_maps( creds )
		if err == nil {
			phost = p.gwid2phost[*uuid]
			err = nil
		}
	}

	return
}

/*
	Crates an array of VM info that can be inserted into the network graph for an entire
	project.
*/
func (p *osif_project) Get_all_info( creds *ostack.Ostack, inc_project bool ) ( ilist []*Net_vm, err error ) {

	err = nil
	ilist = nil

	if p == nil  ||  creds == nil {
		err = fmt.Errorf( "creds were nil" )
		osif_sheep.Baa( 2, "lazy update: unable to get_all: nil creds" )
		return
	}

	if time.Now().Unix() - p.lastfetch > 90 {					// if not fresh force a reload first
		err = p.refresh_maps( creds )
		osif_sheep.Baa( 2, "lazy update: data reload for: get_all" )
		if err != nil {
			return
		}
	}

	
	found := 0
	ilist = make( []*Net_vm, len( p.ip2vmid ) )

	for k, _ := range p.ip2vmid {
		name := p.ip2vm[k]
		_, id, ip4, fip4, mac, gw, _, phost, gwmap, _, lerr := p.Get_info( &k, creds, true )
		if lerr == nil  {
			if name == nil {
				n := "unknown"
				name = &n
			}
			ilist[found] = Mk_netreq_vm( name, id, ip4, nil, phost, mac, gw, nil, fip4, gwmap )
			found++
		}  
	}

	pname, _ := creds.Get_project() 
	osif_sheep.Baa( 1, "get all osvm info found %d VMs in %s", found, *pname )

	return
}

/* Public interface to get the default gateway (router) for a project. Causes data to 
	be loaded if stale.  Search is the project name or ID and can be of the form 
	project/<stuff> where stuff will be ignored. New data (return) is true if the data
	had to be loaded.
*/
func (p *osif_project) Get_default_gw( search *string, creds *ostack.Ostack, inc_project bool ) ( gw *string, new_data bool, err error ) {

	new_data = false
	err = nil
	gw = nil

	if creds == nil {
		err = fmt.Errorf( "creds were nil" )
		osif_sheep.Baa( 1, "lazy gw update: unable to get, nil creds" )
		return
	}

	if time.Now().Unix() - p.lastfetch < 90 {					// if fresh, try to avoid reload
		gw = p.suss_default_gw( search )
	}

	if gw == nil {											// not found or not fresh, force reload
		osif_sheep.Baa( 2, "lazy gw update: data reload for: %s", *p.name )
		new_data = true		
		err = p.refresh_maps( creds )
		if err == nil {
			gw = p.suss_default_gw( search )
		}
	}

	return
}

/*
	Fill in the ip2mac map that is passed in with ours. Must grab the read lock to make this
	safe.
*/
func (p *osif_project) Fill_ip2mac( umap map[string]*string ) {
	if umap == nil {
		return
	}

	p.rwlock.RLock()							// lock for reading
	defer p.rwlock.RUnlock() 					// ensure unlocked on return

	for k, v := range p.ip2mac {
		umap[k] = v
	}
}

// ------------------ utility ------------------------------------------------------------------------------

/*
	Given a project id (or project name) return the project info data and creds.
	project info is indexed by the id, and creds are indexed by the name (TODO change 
	creds to be indexed by id). We return the pid on the off chance that the name 
	was passed in and the caller needs the real id.
*/
func pid2data( pid *string, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) ( 
	p *osif_project, rpid *string, creds *ostack.Ostack, err error ) {
	orig := pid								// hold for error message if needed

	if pid == nil {
		osif_sheep.Baa( 1, "pid2data project id passed in was nil" )
		err = fmt.Errorf( "missing project id" ) 
		return
	}

	pname := id2pname[*pid]
	if pname == nil {						// [0] should be an id, but allow for a name to be sent in
		pname = pid
		pid = pname2id[*pname]
	}

	if pid == nil {
		osif_sheep.Baa( 1, "pid2data project id passed in could not be mapped to a name, or name to project: %s", *orig )
		if osif_sheep.Get_level( ) > 1 {
			for k, v := range id2pname {
				osif_sheep.Baa( 2, "id2pname[%s] == %s", k, *v )
			}
		}
		err = fmt.Errorf( "%s could not be mapped to a project name", *orig ) 
		return
	}

	p = os_projs[*pid]
	if p == nil {
		osif_sheep.Baa( 1, "pid2data project id could not be mapped to os_proj entry: %s", *orig )
		if osif_sheep.Get_level( ) > 1 {
			for k := range os_projs {
				osif_sheep.Baa( 2, "os_proj[%s] exists", k )
			}
		}
		err = fmt.Errorf( "%s could not be mapped to a osif_project", *orig ) 
		return
	}

	creds = os_refs[*pname]
	if creds == nil {
		osif_sheep.Baa( 1, "pid2data project id could not be mapped to os_refs entry (creds): %s", *orig )
		if osif_sheep.Get_level( ) > 1 {
			for k, v := range os_refs {
				osif_sheep.Baa( 2, "os_refs[%s] expired: %v", k, v.Is_expired() )
			}
		}
		err = fmt.Errorf( "%s could not be mapped to a openstack credentials", *orig )
		p = nil
		return
	}

	rpid = pid
	err = nil
	return
}
// - - - - - -   gorouintes kicked by main osif goroutine to run asynch  - - - - - - - - - - - - - - - - - - - - - - - - - - - -

/*
	Get openstack host information.
	Given a project-id/host as input, dig out all of the host's information and build a struct
	that can be passed into the network manager as an add host to graph request. This
	expects to run as a go routine and to write the response directly back on the channel
	givn in the message block.
*/
func get_os_hostinfo( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	if msg == nil || msg.Response_ch == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil

	tokens := strings.Split( msg.Req_data.( string ), "/" )			// break project/host into bits
	if len( tokens ) != 2 || tokens[0] == "" || tokens[1] == "" {
		osif_sheep.Baa( 1, "get hostinfo: unable to map to a project: %s bad tokens",  *(msg.Req_data.( *string )) )
		msg.State = fmt.Errorf( "invalid project/hostname string: %s", *(msg.Req_data.( *string )) )
		msg.Response_ch <- msg
		return
	}

	if tokens[0] == "!" { 					// !//ipaddress was given; we've got nothing, so bail now
		osif_sheep.Baa( 1, "get hostinfo: unable to map to a project: %s lone bang",  *(msg.Req_data.( *string )) )
		msg.Response_ch <- msg
		return
	}

	p, pid, creds, err := pid2data( &tokens[0], os_refs, os_projs, id2pname, pname2id )
	if err != nil {
		osif_sheep.Baa( 1, "get host info: unable to map to a project: %s: %s",  *(msg.Req_data.( *string )), err )  
		msg.State = err
		msg.Response_ch <- msg
		return
	}

	osif_sheep.Baa( 2, "lazy update: get host info setup complete for (%s) %s", tokens[0], msg.Req_data.( string ) )

	search := *pid + "/" + tokens[1]							// search string must be id/hostname
	name, id, ip4, fip4, mac, gw, cidr, phost, gwmap, _, err := p.Get_info( &search, creds, true )
	if err != nil {
		msg.State = fmt.Errorf( "unable to retrieve host info: %s", err )
		msg.Response_ch <- msg
		return
	}
	
	msg.Response_data = Mk_netreq_vm( name, id, ip4, nil, phost, mac, gw, cidr, fip4, gwmap )		// build the vm data block for network manager
	msg.Response_ch <- msg																// and send it on its merry way

	return
}

/*
	Invoked as a goroutine (asynch) to dig the needed subnet information from the current project block
	and return it on the message channel indicated by the message. If the project block info is stale
	then it is reloaded from openstack.  The data contained in the message is expected to be a string
	which is the project and subnet (in that order) separated by a space. On success, the message 
	reponse contains a pointer to the subnet_info structure containing the data. 
*/
func get_sninfo( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	var err error

	if msg == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil

	tokens := strings.Split( msg.Req_data.( string ), " " )			// we expect data to be project subnet separated by a space
	if len( tokens ) != 2 || tokens[0] == "" || tokens[1] == "" {
		msg.State = fmt.Errorf( "(%s) is not project subnet", msg.Req_data.( string ) )
		msg.Response_ch <- msg
		return
	}

	p, _, creds, err := pid2data( &tokens[0], os_refs, os_projs, id2pname, pname2id )
	if err != nil {
		osif_sheep.Baa( 1, "get sn info: unable to map to a project: %s: %s",  msg.Req_data.( string ), err )  
		msg.State = err
		msg.Response_ch <- msg
		return
	}

	si := &Subnet_info{}
	si.uuid = &tokens[1]
	si.name, si.project, si.cidr, si.ip, si.phost, err = p.get_sninfo( &tokens[1], creds ) 	// dig out the subnet info
	if err == nil  &&  si.name != nil {
		si.token = creds.Get_token()			// get our admin token which needs to be passed to the agent
		msg.State = nil 
		msg.Response_data = si
		//osif_sheep.Baa( 1, ">>>> got subnet info:  %s %s %s %s %s", *si.name, *si.project, *si.cidr, *si.ip, *si.phost )
	} else {
		msg.State = fmt.Errorf( "could not find subnet info for: %s", tokens[1] )
	}

	msg.Response_ch <- msg
	return
}


/*
	Invoked as a goroutine (asynch) to dig the physical host value given a gateway (router) id.
	The string is returned on the message channel indicated in the message.  If the project info
	data is stale, it is reloaded.
*/
func osif_gw2phost( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	var err error

	if msg == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil

	tokens := strings.Split( msg.Req_data.( string ), " " )			// we expect data to be project gateway id, separated by a space
	if len( tokens ) != 2 || tokens[0] == "" || tokens[1] == "" {
		msg.State = fmt.Errorf( "project subnet string" )
		msg.Response_ch <- msg
		return
	}

	p, _, creds, err := pid2data( &tokens[0], os_refs, os_projs, id2pname, pname2id )
	if err != nil {
		osif_sheep.Baa( 1, "get sn info: unable to map to a project: %s: %s",  *(msg.Req_data.( *string )), err )  
		msg.State = err
		msg.Response_ch <- msg
		return
	}

	phost, err := p.gw2phost( &tokens[1], creds )			// dig from exising info, or request update from openstack if stale
	if err == nil  &&  phost != nil {
		msg.State = nil 
		msg.Response_data = *phost
	} else {
		if err != nil {
			msg.State = fmt.Errorf( "could not find phost for: %s: %s", tokens[1], err )
		} else {
			msg.State = fmt.Errorf( "could not find phost for: %s: no diagnostic given", tokens[1] )
		}
	}

	msg.Response_ch <- msg
	return
}


/* Get the default gateway for a project. Returns the string directly to the channel 
	that send the osif the message. Expects to be executed as  a go routine. 
go get_os_defgw( msg, os_refs, os_projects, id2pname, pname2id )			// do it asynch and return the result on the message channel
*/
func get_os_defgw( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	if msg == nil || msg.Response_ch == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil

	if msg.Req_data != nil {
		tokens := strings.Split( *(msg.Req_data.( *string )), "/" )			// split off /junk if it's there
		if tokens[0] == "!" || tokens[0] == "" { 							// nothing to work with; bail now
			osif_sheep.Baa( 1, "get_defgw: unable to map to a project -- bad token[0] --: %s",  *(msg.Req_data.( *string )) )
			msg.Response_ch <- msg
			return
		}

		if tokens[0][0:1] == "!" {				// first character is a bang, but there is a name/id that follows
			tokens[0] = tokens[0][1:]			// ditch the bang and go on
		}

		pid := &tokens[0]
		pname := id2pname[*pid]
		if pname == nil {						// it should be an id, but allow for a name/host to be sent in
			osif_sheep.Baa( 1, "get_defgw: unable to map to a project -- no pname --: %s",  *(msg.Req_data.( *string )) )
			pname = &tokens[0]
			pid = pname2id[*pname]
		}

		if pid == nil {
			osif_sheep.Baa( 1, "get_defgw: unable to map to a project: %s",  *(msg.Req_data.( *string )) )
			msg.State = fmt.Errorf( "%s could not be mapped to a osif_project", *(msg.Req_data.( *string )) )
			msg.Response_ch <- msg
			return
		}

		p := os_projs[*pid]						// finally we can find the data associated with the project; maybe
		if p == nil {
			osif_sheep.Baa( 1, "get_defgw: unable to map project to data: %s", *pid )
			msg.State = fmt.Errorf( "%s could not be mapped to a osif_project", *(msg.Req_data.( *string )) )
			msg.Response_ch <- msg
			return
		}

		creds := os_refs[*pname]
		if creds == nil {
			msg.State = fmt.Errorf( "defgw: %s could not be mapped to openstack creds ", *pname )
			msg.Response_ch <- msg
			return
		}

		msg.Response_data, _, msg.State = p.Get_default_gw( pid, creds, true )
		msg.Response_ch <- msg
		return
	}
	
	osif_sheep.Baa( 1, "get_defgw:  missing data (nil) in request" )
	msg.State = fmt.Errorf( "defgw: missing data in request" )
	msg.Response_ch <- msg																// and send it on its merry way

	return
}

/*
	Get a complete list of VMs for a project as network request blocks so they can be added. The project name is 
	expected to be in the request data (*string) and can be either the name or the project id.


	pid is a pointer to either the project name or the project ID. 
	Returns an array of net_vm struts that can be passed to network manager to insert into the graph.
*/
func get_projvm_info( pid *string, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) ( ilist []*Net_vm, err error ) {
																	// must translate to project name to be able to find creds
	ilist = nil
	err = nil

	pname := id2pname[*pid]											// if id passed in
	if pname == nil {
		pname = pid													// assume it's the name since it didn't translate as an ID
		pid = pname2id[*pname]					
	}

	if pid == nil {
		osif_sheep.Baa( 1, "projvm_info: could not translate project name to a project id: %s", *pname )
		err = fmt.Errorf( "projvm_info: %s could not be mapped to a project id", *pname )
		return
	}

	p := os_projs[*pid]
	if p == nil {
		osif_sheep.Baa( 1, "projvm_info: could not translate project name to a project struct: %s", *pname )
		err = fmt.Errorf( "projvm_info: %s could not be mapped to a project struct", *pname )
		return
	}

	creds := os_refs[*pname]	
	if creds == nil {
		osif_sheep.Baa( 1, "projvm_info: project did not translate to openstack creds: %s", *pname )
		err = fmt.Errorf( "projvm_info: project did not translate to openstack creds: %s", *pname )
		return
	}

	ilist, err =  p.Get_all_info( creds, true )		// finally have enough info to dig 
	return
}

/*
	Gathers the VM information for all VMs in one or more projects. If "_all_proj" is given as the project name then
	all projects  known to Tegu are fetched. 

	Expected to execute as a go routine and writes the resulting array to the channel specified in the message.
*/
func get_all_osvm_info( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	if msg == nil || msg.Response_ch == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil
	msg.State = nil

	if msg.Req_data == nil {
		osif_sheep.Baa( 1, "osvm_info: request data didn't contain a project name or ID" )
		msg.State = fmt.Errorf( "osvm_info: request data didn't contain a project name or ID" )
		msg.Response_ch <- msg
		return
	}

	pid := msg.Req_data.( *string )
	if *pid == "_all_proj" {
		ilist := make( []*Net_vm, 0 )
		for k := range os_refs {
			if k != "_ref_" {
				nlist, err := get_projvm_info( &k, os_refs, os_projs, id2pname, pname2id )					// dig out next project's stuff
				if err == nil {
					llist := make( []*Net_vm, len( ilist ) + len( nlist ) )									// create array large enough
					copy( llist[:], ilist[:] )																// copy contents into new array
					copy( llist[len( ilist):], nlist[:] )
					ilist = llist
				} else {
					osif_sheep.Baa( 1, "osvm_info: could not dig out VM information for project: %s: %s", k, err )
				}
			}
		}

		msg.Response_data = ilist
		if len( ilist ) <= 0 {
			msg.State = fmt.Errorf( "osvm_info: unable to dig any information for all projects" )
		} else {
			msg.State = fmt.Errorf( "osvm_info: fetched info for all projects: %d elements", len( ilist ) )
			msg.State = nil
		}
	} else {
		msg.Response_data, msg.State = get_projvm_info( pid, os_refs, os_projs, id2pname, pname2id )		// just dig out for the one project
	}

	msg.Response_ch <- msg
}

