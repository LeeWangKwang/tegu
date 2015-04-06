
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
				16 Jan 2015 - Support port masks in flow-mods.
				01 Apr 2015 - Added ipv6 support for finding gateway/routers.
*/

package managers
// should this move to gizmos?

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"codecloud.web.att.com/gopkgs/ipc"
	"codecloud.web.att.com/gopkgs/ostack"
)


type osif_project struct {
	name		*string
	lastfetch	int64						// timestamp of last map update to detect freshness
	vmid2ip		map[string]*string			// translation maps for the project
	ip2vmid		map[string]*string
	ip2vm		map[string]*string
	vm2ip		map[string]*string
	vmid2host	map[string]*string
	ip2mac		map[string]*string
	gwmap		map[string]*string
	ip2fip		map[string]*string
	fip2ip		map[string]*string
	gw2cidr		map[string]*string

	rwlock		sync.RWMutex						// must lock to prevent update collisions
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
if osif_sheep.Would_baa( 2 ) {
for k, v := range p.ip2vmid {
osif_sheep.Baa( 2, "   %s -> %s", k, *v )
}
}
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
	

		gwmap, _, _, _, _, _, err := creds.Mk_gwmaps( nil, nil, nil, nil, nil, nil, true, false )		
		if err != nil {
			osif_sheep.Baa( 2, "WRN: unable to map gateway info: %s; %s   [TGUOSI006]", creds.To_str( ), err )
			creds.Expire()					// force re-auth next go round
		} else {
			osif_sheep.Baa( 2, "%s map sizes: gwmap=%d", *p.name, len( gwmap ) )
			if len( gwmap ) > 0 {
				p.gwmap = gwmap
			}
		}

		_, gw2cidr, err := creds.Mk_snlists( ) 		// get list of gateways and their subnet cidr
		if err == nil && gw2cidr != nil {
			p.gw2cidr = gw2cidr
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
					osif_sheep.Baa( 1, "mapped ip to gateway for: %s  %s", *ip, k )
					return &k
				}
		}
	}

	osif_sheep.Baa( 1, "osif-ip2gw: unable to map ip to gateway for: %s", *ip )
	return nil
}

/*
	Supports Get_info by searching for the information but does not do a reload.
*/
func (p *osif_project) suss_info( search *string ) ( name *string, id *string, ip4 *string, fip4 *string, mac *string, gw *string, phost *string, gwmap map[string]*string ) {

	name = nil
	id = nil
	ip4 = nil

	if p == nil || search == nil {
		return
	}

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
		name, id, ip4, fip4, mac, gw, phost, gwmap = p.suss_info( search )
	}

	if name == nil {											// not found or not fresh, force reload
		osif_sheep.Baa( 2, "lazy update: data reload for: %s", *p.name )
		new_data = true		
		err = p.refresh_maps( creds )
		if err == nil {
			name, id, ip4, fip4, mac, gw, phost, gwmap = p.suss_info( search )
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


// - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -

/*
	Given a project-id/host as input, dig out all of the host's information and build a struct
	that can be passed into the network manager as an add host to graph request. This
	expects to run as a go routine and to write the response directly back on the channel
	givn in the message block.
*/
func get_hostinfo( msg	*ipc.Chmsg, os_refs map[string]*ostack.Ostack, os_projs map[string]*osif_project, id2pname map[string]*string, pname2id map[string]*string ) {
	if msg == nil {
		return															// prevent accidents
	}

	msg.Response_data = nil

	tokens := strings.Split( *(msg.Req_data.( *string )), "/" )			// break project/host into bits
	if len( tokens ) != 2 || tokens[0] == "" || tokens[1] == "" {
		msg.State = fmt.Errorf( "invalid project/hostname string" )
		msg.Response_ch <- msg
		return
	}

	if tokens[0] == "!" { 					// !//ipaddress was given; we've got nothing, so bail now
		msg.Response_ch <- msg
		return
	}

	if tokens[0][0:1] == "!" {				// first character is a bang, but there is a name/id that follows
		tokens[0] = tokens[0][1:]			// ditch it for this
	}

	pid := &tokens[0]
	pname := id2pname[*pid]
	if pname == nil {						// it should be an id, but allow for a name/host to be sent in
		pname = &tokens[0]
		pid = pname2id[*pname]
	}

	if pid == nil {
		osif_sheep.Baa( 1, "get hostinfo: unable to map to a project: %s",  *(msg.Req_data.( *string )) )  // might be !project/vm, and so this is ok
		msg.State = fmt.Errorf( "%s could not be mapped to a osif_project", *(msg.Req_data.( *string )) )
		msg.Response_ch <- msg
		return
	}

	p := os_projs[*pid]
	if p == nil {
		msg.State = fmt.Errorf( "%s could not be mapped to a osif_project", *(msg.Req_data.( *string )) )
		msg.Response_ch <- msg
		return
	}

	creds := os_refs[*pname]
	if creds == nil {
		msg.State = fmt.Errorf( "%s could not be mapped to openstack creds ", *pname )
		msg.Response_ch <- msg
		return
	}
	
	osif_sheep.Baa( 2, "lazy update: get host info setup complete for (%s) %s", *pname, *(msg.Req_data.( *string )) )

	search := *pid + "/" + tokens[1]							// search string must be id/hostname
	name, id, ip4, fip4, mac, gw, phost, gwmap, _, err := p.Get_info( &search, creds, true )
	if err != nil {
		msg.State = fmt.Errorf( "unable to retrieve host info: %s", err )
		msg.Response_ch <- msg
		return
	}
	
	msg.Response_data = Mk_netreq_vm( name, id, ip4, nil, phost, mac, gw, fip4, gwmap )		// build the vm data block for network manager
	msg.Response_ch <- msg																// and send it on its merry way

	return
}
