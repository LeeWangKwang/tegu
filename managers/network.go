// vi: sw=4 ts=4:
/*
 ---------------------------------------------------------------------------
   Copyright (c) 2013-2015 AT&T Intellectual Property

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at:

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
 ---------------------------------------------------------------------------
*/


/*

	Mnemonic:	network
	Abstract:	Manages everything associated with a network. This module contains a
				goroutine which should be invoked from the tegu main and is responsible
				for managing the network graph and responding to requests for information about
				the network graph. As a part of the collection of functions here, there is also
				a tickler which causes the network graph to be rebuilt on a regular basis.

				The network manager goroutine listens to a channel for requests such as finding
				and reserving a path between two hosts, and generating a json representation
				of the network graph for outside consumption.

				TODO: need to look at purging links/vlinks so they don't bloat if the network changes

	Date:		24 November 2013
	Author:		E. Scott Daniels

	Mods:		19 Jan 2014 - Added support for host-any reservations.
				11 Feb 2014 - Support for queues on links rather than just blanket obligations per link.
				21 Mar 2014 - Added noop support to allow main to hold off driving checkpoint
							loading until after the driver here has been entered and thus we've built
							the first graph.
				03 Apr 2014 - Support for endpoints on the path.
				05 May 2014 - Added support for merging gateways into the host list when not using floodlight.
				18 May 2014 - Changes to allow cross tenant reservations.
				30 May 2014 - Corrected typo in error message
				11 Jun 2014 - Added overall link-headroom support
				25 Jun 2014 - Added user level link capacity limits.
				26 Jun 2014 - Support for putting vmid into graph and hostlist output.
				29 Jun 2014 - Changes to support user link limits.
				07 Jul 2014 - Added support for reservation refresh.
				15 Jul 2014 - Added partial path allocation if one endpoint is in a different user space and
							is not validated.
				16 Jul 2014 - Changed unvalidated indicator to bang (!) to avoid issues when
					vm names have a dash (gak).
				29 Jul 2014 - Added mlag support.
				31 Jul 2014 - Corrected a bug that prevented using a VM ID when the project name/id was given.
				11 Aug 2014 - Corrected bleat message.
				01 Oct 2014 - Corrected bleat message during network build.
				09 Oct 2014 - Turned 'down' two more bleat messages to level 2.
				10 Oct 2014 - Correct bi-directional link bug (228)
				30 Oct 2014 - Added support for !//ex-ip address syntax, corrected problem with properly
					setting the -S or -D flag for an external IP (#243).
				12 Nov 2014 - Change to strip suffix on phost map.
				17 Nov 2014 - Changes for lazy mapping.
				24 Nov 2014 - Changes to drop the requirement for both VMs to have a floating IP address
					if a cross tenant reservation is being made. Also drops the requirement that the VM
					have a floating IP if the reservation is being made with a host using an external
					IP address.
				11 Mar 2015 - Corrected bleat messages in find_endpoints() that was causing core dump if the
					g1/g2 information was missing. Corrected bug that was preventing uuid from being used
					as the endpoint 'name'.
				20 Mar 2014 - Added REQ_GET_PHOST_FROM_MAC code
				25 Mar 2015 - IPv6 changes.
				30 Mar 2014 - Added ability to accept an array of Net_vm blocks.
				18 May 2015 - Added discount support.
				26 May 2015 - Conversion to support pledge as an interface.
				08 Jun 2015 - Added support for dummy star topo.
					Added support for 'one way' reservations (non-virtual router so no real endpoint)
				16 Jun 2015 - Corrected possible core dump in host_info() -- not checking for nil name.
				18 Jun 2015 - Added oneway rate limiting and delete support.
 				02 Jul 2015 - Extended the physical host refresh rate.
				03 Sep 2015 - Correct nil pointer core dump cause.
				06 Oct 2015 - Revammp to use endpoints (uuid based) rather than hosts (IP addresses).
				16 Dec 2015 - Strip domain name when we create the vm to phost map since openstack sometimes
					gives us fqdns and sometimes not, but we only ever get hostname from the topo side.
				17 Dec 2015 - Correct nil pointer crash trying to fill in the vm map.
*/

package managers

import (
	"fmt"
	"os"
	"strings"

	"github.com/att/gopkgs/bleater"
	"github.com/att/gopkgs/clike"
	"github.com/att/gopkgs/ipc"
	"github.com/att/gopkgs/ipc/msgrtr"

	"github.com/att/tegu/gizmos"
	"github.com/att/tegu/datacache"
)

// -- configuration management -----------------------------------------------------------
/*
	All needed config information pulled from the config file.
*/

type net_cfg struct {
	discount		int64		// discount applied to all bw reservations
	relaxed			bool		// run in relaxed mode
	refresh			int			// refresh rate for network topp
	max_link_cap	int64		// ???
	bleat_level		int			// how chatty network will be
	mlag_paths		bool		// use mlag paths when building bw reservations?
	find_all_paths	bool		// find all paths to a destination
	link_headroom	int			// percentage we do not use on any link
	link_alarm_thresh	int		// percentage threshold
	def_ul_cap		int64		// user link capacity default value
	phost_suffix	*string		// if fqmgr is adding a suffix we need to strip in some cases
	topo_file		*string
}

/*
	Suss out things we need from the config data and build a new struct.
*/
func mk_net_cfg( cfg_data map[string]map[string]*string ) ( nc *net_cfg ) {

	net_sheep.Baa( 1, "reloading configuration file information" )
	nc = &net_cfg {
		discount:	0,
		relaxed:	false,
		refresh:	30,
		max_link_cap: 0,
		bleat_level:	1,
		mlag_paths:		true,
		find_all_paths:	false,
		link_headroom:	0,
		link_alarm_thresh: 0,
		def_ul_cap:		-1,
		phost_suffix:	nil,
	}

	if cfg_data["default"] != nil {								// things we pull from the default section
			nc.topo_file = cfg_data["default"]["static_phys_graph"]
	}

	if cfg_data["fqmgr"] != nil {								// we need to know if fqmgr is adding a suffix to physical host names so we can strip
		if p := cfg_data["fqmgr"]["phost_suffix"]; p != nil {
			nc.phost_suffix = p
			net_sheep.Baa( 1, "will strip suffix from mac2phost map: %s", *nc.phost_suffix )
		}	
	}

	if cfg_data["network"] != nil {
		if p := cfg_data["network"]["discount"]; p != nil {
			d := clike.Atoll( *p ); 			
			if d < 0 {
				nc.discount = 0
			} else {
				nc.discount = d
			}
		}

		if p := cfg_data["network"]["relaxed"]; p != nil {
			nc.relaxed = *p ==  "true" || *p ==  "True" || *p == "TRUE"
		}
		if p := cfg_data["network"]["refresh"]; p != nil {
			nc.refresh = clike.Atoi( *p ); 			
		}
		if p := cfg_data["network"]["link_max_cap"]; p != nil {
			nc.max_link_cap = clike.Atoi64( *p )
		}
		if p := cfg_data["network"]["verbose"]; p != nil {
			net_sheep.Set_level(  uint( clike.Atoi( *p ) ) )
		}

		if p := cfg_data["network"]["all_paths"]; p != nil {
			nc.find_all_paths = false
			net_sheep.Baa( 0, "config file key find_all_paths is deprecated: use find_paths = {all|mlag|shortest}" )
		}

		if p := cfg_data["network"]["find_paths"]; p != nil {
			switch( *p ) {
				case "all":	
					nc.find_all_paths = true
					nc.mlag_paths = false

				case "mlag":
					nc.find_all_paths = false
					nc.mlag_paths = true

				case "shortest":
					nc.find_all_paths = false
					nc.mlag_paths = false

				default:
					net_sheep.Baa( 0, "WRN: invalid setting in config: network:find_paths %s is not valid; must be: all, mlag, or shortest; assuming mlag  [TGUNET010]", *p )
					nc.find_all_paths = false
					nc.mlag_paths = true
			}
		}

		if p := cfg_data["network"]["link_headroom"]; p != nil {
			nc.link_headroom = clike.Atoi( *p )							// percentage that we should take all link capacities down by
		}

		if p := cfg_data["network"]["link_alarm"]; p != nil {
			nc.link_alarm_thresh = clike.Atoi( *p )						// percentage of total capacity when an alarm is generated
		}

		if p := cfg_data["network"]["user_link_cap"]; p != nil {
			nc.def_ul_cap = clike.Atoi64( *p )
		}
	}

	return
}

// --------------------------------------------------------------------------------------

// this probably should be network rather than Network as it's used only internally

/*
	Defines everything we need to know about a network.
*/
type Network struct {				
	switches	map[string]*gizmos.Switch	// symtable of switches
	links		map[string]*gizmos.Link		// table of links allows for update without resetting allotments
	vlinks		map[string]*gizmos.Link		// table of virtual links (links between ports on the same switch)
	endpts		map[string]*gizmos.Endpt	// endpoints (used to be known as hosts)
	limits		map[string]*gizmos.Fence	// user boundary defaults for per link caps
	mlags		map[string]*gizmos.Mlag		// reference to each mlag link group by name
	relaxed		bool						// if true, we're in relaxed mode which means we don't path find or do admission control.
	ep_cache	map[string]*gizmos.Endpt	// cache if called before we have a topo
	ep_good		bool						// set true when we have processed a valid endpoint list
}

// ------------ private -------------------------------------------------------------------------------------

/*
	constructor
*/
func mk_network( mk_links bool ) ( n *Network ) {
	n = &Network { }
	n.switches = make( map[string]*gizmos.Switch, 20 )		// initial sizes aren't limits, but might help save space

	if mk_links {
		n.links = make( map[string]*gizmos.Link, 2048 )		// must maintain a list of links so when we rebuild we preserve obligations
		n.vlinks = make( map[string]*gizmos.Link, 2048 )
		n.mlags = make( map[string]*gizmos.Mlag, 2048 )
	}

	return
}

// -------------- map management (mostly tegu-lite) -----------------------------------------------------------
/*
	Set the relaxed mode in the network.
*/
func (n *Network) Set_relaxed( state bool ) {
	if n != nil {
		n.relaxed = state
	}
}

/*
	Using a net_vm struct update the various maps. Allows for lazy discovery of
	VM information rather than needing to request everything all at the same time.
*/
func (net *Network) insert_vm( vm *Net_vm ) {
	net_sheep.Baa( 1, "#### insert_vm called and is depreacated -- ignored" )
	return

/*
	vname, vid, vip4, _, vphost, gw, vmac, vfip := vm.Get_values( )
	if vname == nil || *vname == "" || *vname == "unknown" {								// shouldn't happen, but be safe
		//return
	
	}

	if net.vm2ip == nil {							// ensure everything exists
		net.vm2ip = make( map[string]*string )
	}
	if net.ip2vm == nil {
		net.ip2vm = make( map[string]*string )
	}

	if net.vmid2ip == nil {
		net.vmid2ip = make( map[string]*string )
	}
	if net.ip2vmid == nil {
		net.ip2vmid = make( map[string]*string )
	}

	if net.vmid2phost == nil {
		net.vmid2phost = make( map[string]*string )
	}
	if net.mac2phost == nil {
		net.mac2phost = make( map[string]*string )
	}

	if net.ip2mac == nil {
		net.ip2mac = make( map[string]*string )
	}

	if net.fip2ip == nil {
		net.fip2ip = make( map[string]*string )
	}
	if net.ip2fip == nil {
		net.ip2fip = make( map[string]*string )
	}

	if net.gwmap == nil {
		net.gwmap = make( map[string]*string )
	}

	if net.vmip2gw == nil {
		net.vmip2gw = make( map[string]*string )
	}
	
	if vname != nil {
		net.vm2ip[*vname] = vip4
	}
	
	if vid != nil {
		net.vmid2ip[*vid] = vip4
		if vphost != nil {
			htoks := strings.Split( *vphost, "." )		// strip domain name
			//net.vmid2phost[*vid] = vphost
			net_sheep.Baa( 2, "vm2phost saving %s (%s) for %s", htoks[0], *vphost, *vid )
			net.vmid2phost[*vid] = &htoks[0]
		} else {
			net_sheep.Baa( 2, "vm2phost phys host is nil for %s", *vid )
		}
	}

	if vip4 != nil {
		net.ip2vmid[*vip4] = vid
		net.ip2vm[*vip4] = vname
		net.ip2mac[*vip4] = vmac
		net.ip2fip[*vip4] = vfip
		net.vmip2gw[*vip4] = gw
	}

	if vfip != nil {
		net.fip2ip[*vfip] = vip4
	}

	vgwmap := vm.Get_gwmap()					// don't assume that all gateways are present in every map
	if vgwmap != nil {							// as it may be just related to the VM and not every gateway
		for k, v := range vgwmap {
			net.gwmap[k] = v
		}	
	}
*/
}


/*
	Given a user name find a fence in the table, or copy the defaults and
	return those.  (User name generic which could be openstack project id etc.)
*/
func (n *Network) get_fence( usr *string ) ( *gizmos.Fence ) {
	var (
		fence *gizmos.Fence
	)

	fence = nil
	if usr != nil {
		fence = n.limits[*usr] 								// get the default fence settings for the user or the overall defaults if none supplied for the user
	} else {
		u := "nobody"
		usr = &u
	}

	if fence == nil {
		if n.limits["default"] != nil {
			fence = n.limits["default"].Copy( usr )			// copy the default to pick up the user name and pass that down
			n.limits[*usr] = fence
		} else {
			nm := "default"
			fence = gizmos.Mk_fence( &nm, 100, 0, 0 )		// create a generic default with no limits (100% max)
			n.limits["default"] = fence
			fence = fence.Copy( usr )
		}
	}

	return fence
}


/*
	Accepts a list (string) of queue data information segments (swid/port,res-id,queue,min,max,pri), splits
	the list based on spaces and adds each information segment to the queue map.  If ep_only is true,
	then we drop all middle link queues (priority-in priority-out).

	(Supports gen_queue_map and probably not useful for anything else)
*/
func qlist2map( qmap map[string]int, qlist *string, ep_only bool ) {
	qdata := strings.Split( *qlist, " " )					// tokenise (if multiple swid/port,res-id,queue,min,max.pri)

	if ep_only {
		for i := range qdata  {
			if qdata[i] != ""  &&  strings.Index( qdata[i], "priority-" ) < 0 {
				qmap[qdata[i]] = 1;
			}
		}
	} else {
		for i := range qdata {
			if qdata[i] != "" {
				qmap[qdata[i]] = 1;
			}
		}
	}
}

/*
	Traverses all known links and generates a switch queue map based on the queues set for
	the time indicated by the timestamp passed in (ts).

	If ep_only is set to true, then queues only for endpoints are generated.

	TODO:  this needs to return the map, not an array (fqmgr needs to change to accept the map)
*/
func (n *Network) gen_queue_map( ts int64, ep_only bool ) ( qmap []string, err error ) {
	err = nil									// at the moment we are always successful
	seen := make( map[string]int, 100 )			// prevent dups which occur because of double links

	for _, link := range n.links {				// for each link in the graph
		s := link.Queues2str( ts )
		qlist2map( seen, &s, ep_only )	// add these to the map
	}

	for _, link := range n.vlinks {				// and do the same for vlinks
		s := link.Queues2str( ts )
		qlist2map( seen, &s, ep_only )			// add these to the map
	}

	qmap = make( []string, len( seen ) )
	i := 0
	for data := range seen {
		net_sheep.Baa( 2, "gen_queue_map[%d] = %s", i, data )
		qmap[i] = data
		i++
	}
	net_sheep.Baa( 1, "gen_queue_map: added %d queue tokens to the list (len=%d)", i, len( qmap ) )
	
	return
}

/*
	Given an endpoint's uuid, returns the desired metadata (map) for an endpoint. 
	This accepts either uuid or project/uuid. If the epname is an external (!/) specification
	then no map is returned.
*/
func (n *Network) uuid2ep_meta( epname *string ) ( md map[string]string, err error) {
	err = nil

	if *epname == "" {
		net_sheep.Baa( 1, "internal mishap: bad name passed to ep2meta_data: empty" )
		err = fmt.Errorf( "endpoint unknown: empty name passed to network manager" )
		return nil, err
	}

	if (*epname)[0:2] == "!/" {					// special external name (no project string following !)
		md = make( map[string]string, 1 )
		md["uuid"] = *epname					// dummy map with the !/address as the uuid 
		return	
	}

	tokens := strings.Split( *epname, "/" )		// could be project/uuid or just uuid
	uuid := tokens[0]
	if len( tokens ) > 1  {
		uuid = tokens[1]
	} 

	ep := n.endpts[uuid]
	if ep == nil {
		net_sheep.Baa( 2, "endpoint not found: %s", *epname )
		err = fmt.Errorf( "endpoint unknown: %s", *epname )
		return
	}

	return ep.Get_meta_copy(), nil		// returns a map of nearly all the data (the switch pointer is not included)
}

/*
	Defrock the epname and verifiy that we have that name in the list.  Returns the name if 
	it is in the list, or if it's !/junk. If the name isn't in the list, an empty string
	is returned.
*/
func (n *Network) defrock_epname( epname *string ) ( string ) {
	if *epname == "" {
		net_sheep.Baa( 1, "internal mishap: bad name passed to ep2meta_data: empty" )
		return ""
	}

	if (*epname)[0:2] == "!/" {					// special external name (no project string following !)
		return	*epname
	}

	tokens := strings.Split( *epname, "/" )		// could be project/uuid or just uuid
	uuid := tokens[0]
	if len( tokens ) > 1  {
		uuid = tokens[1]
	} 

	net_sheep.Baa( 2, "defrocking: %s uuid=%s (%d)", *epname, uuid, len( n.endpts ) )
	if net_sheep.Would_baa( 3 ) {
		for _, v := range n.endpts {
			net_sheep.Baa( 3, "defrocking: %s", v )
		}
	}

	if n.endpts[uuid] != nil {
		return uuid
	}

	return ""
}
	

/*
	DEPRECATED
	Returns the ip address associated with the name. The name may indeed be
	an IP address which we'll look up in the hosts table to verify first.
	If it's not an ip, then we'll search the vm2ip table for it.

	If the name passed in has a leading bang (!) meaning that it was not
	validated, we'll strip it and do the lookup, but will return the resulting
	IP address with a leading bang (!) to propagate the invalidness of the address.

	The special case !/ip-address is used to designate an external address. It won't
	exist in our map, and we return it as is.
func (n *Network) name2ip( hname *string ) (ip *string, err error) {
	ip = nil
	err = nil
	lname := *hname								// lookup name - we may have to strip leading !

	if *hname == "" {
		net_sheep.Baa( 1, "bad name passed to name2ip: empty" )
		err = fmt.Errorf( "host unknown: empty name passed to network manager", *hname )
		return
	}

	if (*hname)[0:2] == "!/" {					// special external name (no project string following !)
		ip = hname
		return
	}

	if  (*hname)[0:1] == "!" {					// ignore leading bang which indicate unvalidated IDs
		lname = (*hname)[1:]
	}

	if n.hosts[lname] != nil {					// we have a host by 'name', then 'name' must be an ip address
		ip = hname
	} else {
		ip = n.vm2ip[lname]						// it's not an ip, try to translate it as either a VM name or VM ID
		if ip == nil {							// maybe it's just an ID, try without
			tokens := strings.Split( lname, "/" )				// could be project/uuid or just uuid
			lname = tokens[len( tokens ) -1]	// local name is the last token
			ip = n.vmid2ip[lname]				// see if it maps to an ip
		}
		if ip != nil {							// the name translates, see if it's in the known net
			if n.hosts[*ip] == nil {			// ip isn't in the network scope as a host, return nil
				err = fmt.Errorf( "host unknown: %s maps to an IP, but IP not known to SDNC: %s", *hname, *ip )
				ip = nil
			} else {
				if (*hname)[0:1] == "!" {					// ensure that we return the ip with the leading bang
					lname = "!" + *ip
					ip = &lname
				}
			}
		} else {
			err = fmt.Errorf( "host unknown: %s could not be mapped to an IP address", *hname )
		}
	}

	return
}
*/

/*
	Given an endpoint address, suss out the 'default' IP  address. Probalby not what is wanted, but 
	in cases where we have nothing to go on, it might work.
*/
func (n *Network) epid2ip( epid *string ) (ip *string, err error) {
	ip = nil
	err = nil

	ep := n.endpts[ *epid ]
	if ep == nil {
		return nil, fmt.Errorf( "cannot map endpoint ID to IP address" )
	}

	ip, _ = ep.Get_addresses( )

	return
}

/*
	Given two switch names see if we can find an existing link in the src->dest direction
	if lnk is passed in, that is passed through to Mk_link() to cause lnk's obligation to be
	'bound' to the link that is created here.

	If the link between src-sw and dst-sw is not there, one is created and added to the map.

	Mlag is a pointer to the string which is the name of the mlag group that this link belongs to.

	We use this to reference the links from the previously created graph so as to preserve obligations.
	(TODO: it would make sense to vet the obligations to ensure that they can still be met should
	a switch depart from the network.)
*/
func (n *Network) find_link( ssw string, dsw string, capacity int64, link_alarm_thresh int, mlag  *string, lnk ...*gizmos.Link ) (l *gizmos.Link) {

	id := fmt.Sprintf( "%s-%s", ssw, dsw )
	l = n.links[id]
	if l != nil {
		if lnk != nil {										// dont assume that the links shared the same allotment previously though they probably do
			l.Set_allotment( lnk[0].Get_allotment( ) )
		}
		return
	}

	net_sheep.Baa( 3, "making link: %s", id )
	if lnk == nil {
		l = gizmos.Mk_link( &ssw, &dsw, capacity, link_alarm_thresh, mlag );	
	} else {
		l = gizmos.Mk_link( &ssw, &dsw, capacity, link_alarm_thresh, mlag, lnk[0] );	
	}

	if mlag != nil {
		ml := n.mlags[*mlag]
		if ml == nil {
			n.mlags[*mlag] = gizmos.Mk_mlag( mlag, l.Get_allotment() )		// creates the mlag group and adds the link
		} else {
			n.mlags[*mlag].Add_link( l.Get_allotment() ) 					// group existed, just add the link
		}
	}

	n.links[id] = l
	return
}

/*
	Looks for a virtual link on the switch given between ports 1 and 2.
	Returns the existing link, or makes a new one if this is the first.
	New vlinks are stashed into the vlink hash.

	We also create a virtual link on the endpoint between the switch and
	the host. In this situation there is only one port (p2 expected to be
	negative) and the id is thus just sw.port.

	m1 and m2 are the mac addresses of the hosts; used to build different
	links since their ports will be -128 when not known in advance.
*/
func (n Network) find_vlink( sw string, p1 int, p2 int, m1 *string, m2 *string ) ( l *gizmos.Link ) {
	var(
		id string
	)

	if p2 < 0 {									
		if p2 == p1 {
			id = fmt.Sprintf( "%s.%s.$s", sw, m1, m2 ) 			// late binding, we don't know port, so use mac for ID
		} else {
			id = fmt.Sprintf( "%s.%d", sw, p1 )					// endpoint -- only a forward link to p1
		}
	} else {
		id = fmt.Sprintf( "%s.%d.%d", sw, p1, p2 )
	}

	l = n.vlinks[id]
	if l == nil {
		l = gizmos.Mk_vlink( &sw, p1, p2, int64( 10 * ONE_GIG ) )
		l.Set_ports( p1, p2 )
		n.vlinks[id] = l
	}

	return
}

/*
	Find a virtual link between two switches -- used when in relaxed mode and no real path
	between endpoints is found, but we still need to pretend there is a path. If we don't
	have a link in the virtual table we'll create one and return that.
*/
func (n Network) find_swvlink( sw1 string, sw2 string  ) ( l *gizmos.Link ) {

	id := fmt.Sprintf( "%s.%s", sw1, sw2 ) 			

	l = n.vlinks[id]
	if l == nil {
		l = gizmos.Mk_link( &sw1, &sw2, int64( 10 * ONE_GIG ), 99, nil )		// create the link and add to virtual table
		l.Set_ports( 1024, 1024 )
		n.vlinks[id] = l
	}

	return
}

/*
	Build a new graph of the network.
	Host is the name/ip:port of the host where floodlight is running.
	Old-net is the reference net that we'll attempt to find existing links in.
	Max_capacity is the generic (default) max capacity for each link.

	Tegu-lite:  sdnhost might be a file which contains a static graph, in json form,
	describing the physical network. The string is assumed to be a filename if it
	does _not_ contain a ':'.

	Host list is a list of physical (compute/net) hosts that we need if building a 
	default star topo.  We _might_ be able to pull this list from eps, but it is 
	unclear at this time if we should trust that list to be everything (most concerned
	about network hosts).
*/
func build( old_net *Network, eps map[string]*gizmos.Endpt, cfg *net_cfg, phost_list *string ) (n *Network) {
	var (
		ssw		*gizmos.Switch
		dsw		*gizmos.Switch
		lnk		*gizmos.Link

		links	[]gizmos.FL_link_json			// list of links from floodlight or simulated floodlight source
		err		error
		hr_factor	int64 = 1
		mlag_name	*string = nil
	)

	n = nil

	if cfg.link_headroom > 0 && cfg.link_headroom < 100 {
		hr_factor = 100 - int64( cfg.link_headroom )
	}

	if phost_list != nil && *phost_list != "" {
		links, err = gizmos.Read_json_links( cfg.topo_file )		// build links from the topo file; if empty/missing, we'll generate a dummy next
		if err != nil || len( links ) <= 0 {
			net_sheep.Baa_some( "star", 500, 1, "generating a dummy star topology: json file empty, or non-existent: %s", *cfg.topo_file )
			links = gizmos.Gen_star_topo( *phost_list )		// generate a dummy topo based on the  phys hosts listed in endpoint list
		}
	} else {
		net_sheep.Baa( 0, "no phost_list yet, not parsing topo file" )
		links = nil								// kicks us out later, but must at least create empty topo first
	}

	n = mk_network( old_net == nil )			// new network, need links and mlags only if it's the first network
	n.relaxed = cfg.relaxed
	if old_net == nil {
		old_net = n								// prevents an if around every try to find an existing link.
	} else {
		n.links = old_net.links					// might it be wiser to copy this rather than reference and update the 'live' copy?
		n.endpts = old_net.endpts
		n.ep_cache = old_net.ep_cache

		n.vlinks = old_net.vlinks
		n.mlags = old_net.mlags
		n.relaxed = old_net.relaxed
	}

	if n.endpts == nil {
		net_sheep.Baa( 1, "making endpoint map in network" );
		n.endpts = make( map[string]*gizmos.Endpt )
	}

	if links == nil || len( links ) <= 0 {
		if eps != nil {					// we cannot run the endpoints until we have a valid topo, so cache this list 
			net_sheep.Baa( 2, "caching endpoint list; no topo yet" )
			if n.ep_cache == nil {
				n.ep_cache = eps		// just save it
			} else {
				for k, v := range eps {
					n.ep_cache[k] = v
				}
			}
		}

		return
	}

	if n.ep_cache != nil {					// if there is a cache, make sure we include it
		net_sheep.Baa( 2, "endpoint list was cached" )
		if eps != nil {
			net_sheep.Baa( 2, "merging %d endpoints from cache", len( n.endpts ) )
			for k, v := range eps {
				n.ep_cache[k] = v
			}
		}  else {
			net_sheep.Baa( 2, "cache contains %d elements", len( n.ep_cache ) )
		}

		eps = n.ep_cache
		n.ep_cache = nil
	}

	// FIXME:  if a link drops do we delete it?
	for i := range links {								// parse all links returned from the controller (build our graph of switches and links)
		if links[i].Capacity <= 0 {
			links[i].Capacity = cfg.max_link_cap		// default if it didn't come from the source
		}

		tokens := strings.SplitN( links[i].Src_switch, "@", 2 )	// if the 'id' is host@interface we need to drop interface so all are added to same switch
		sswid := tokens[0]								
		tokens = strings.SplitN( links[i].Dst_switch, "@", 2 ) 
		dswid := tokens[0]

		ssw = n.switches[sswid]
		if ssw == nil {
			ssw = gizmos.Mk_switch( &sswid )
			n.switches[sswid] = ssw
		}

		dsw = n.switches[dswid]
		if dsw == nil {
			dsw = gizmos.Mk_switch( &dswid )
			n.switches[dswid] = dsw
		}

		// omitting the link (last parm) causes reuse of the link if it existed so that obligations are kept; links _are_ created with the interface name
		lnk = old_net.find_link( links[i].Src_switch, links[i].Dst_switch, (links[i].Capacity * hr_factor)/100, cfg.link_alarm_thresh, links[i].Mlag )		
		lnk.Set_forward( dsw )
		lnk.Set_backward( ssw )
		lnk.Set_port( 1, links[i].Src_port )		// port on src to dest
		lnk.Set_port( 2, links[i].Dst_port )		// port on dest to src
		ssw.Add_link( lnk )

		if links[i].Direction == "bidirectional" { 			// add the backpath link
			mlag_name = nil
			if links[i].Mlag != nil {
				mln := *links[i].Mlag + ".REV"				// differentiate the reverse links so we can adjust them with amount_in more easily
				mlag_name = &mln
			}
			lnk = old_net.find_link( links[i].Dst_switch, links[i].Src_switch, (links[i].Capacity * hr_factor)/100, cfg.link_alarm_thresh, mlag_name )
			lnk.Set_forward( ssw )
			lnk.Set_backward( dsw )
			lnk.Set_port( 1, links[i].Dst_port )		// port on dest to src
			lnk.Set_port( 2, links[i].Src_port )		// port on src to dest
			dsw.Add_link( lnk )
			net_sheep.Baa( 4, "build: addlink: src [%d] %s %s", i, links[i].Src_switch, n.switches[sswid].To_json() )
			net_sheep.Baa( 4, "build: addlink: dst [%d] %s %s", i, links[i].Dst_switch, n.switches[dswid].To_json() )
		}
	}

	if len( n.endpts ) > 0 {						// if we build after we have an endpoint list then ok to allow checkpoint to be processed
		n.ep_good = true
	} else {
		n.ep_good = false
	}

	for k, ep := range eps {										// for each end point, add to the graph
		if *(ep.Get_meta_value( "phost" )) == "" {
			delete( n.endpts, k ) 
		} else {
			n.endpts[k] = ep											// reference only by uuid
		}
	}

	for k, ep := range n.endpts {									// switches are generated each go round, so we must insert endpoint into the new set
		swname := ep.Get_phost()
		csw := n.switches[*swname]									// connected switch is switch with the phost
		if csw != nil {
			_, port := ep.Get_switch_port()
			ep.Set_switch( csw, port )								// allows us to find a starting switch by endpoint id for path finding
			csw.Add_endpt( &k, port )								// allows switch to respond to Has_host() call by id or mac

			if net_sheep.Would_baa( 3 ) {
				mac, _ := ep.Get_addresses( )
				net_sheep.Baa( 3, "saving host %s (%s) in switch : %s port: %d", *mac, k, *swname, port )
			}
		} else {
			net_sheep.Baa( 1, "attachment switch for endpoint %s is missing: %s", k, swname )
		}
	}

	return
}

/*
	Accept a list of endpoints and use them to add to or update the current net.
	Returns the new net if opdated and a bool flag to indiceate whether or not 
	the attempt was successful (if not, caller might need to retry later).
*/
func update_net( act_net *Network, epmap map[string]*gizmos.Endpt, cfg *net_cfg, hlist *string ) ( new_net *Network, cached bool ) {

	net_sheep.Baa( 1, "updating network with new eplist (%d elements)", len( epmap ) )
	new_net = build( act_net, epmap, cfg, hlist )				// use map to rebuild the network

	if new_net != nil {
		new_net.xfer_maps( act_net )						// copy maps from old net to the new graph
		net_sheep.Baa( 1, "new network successfully built" )
	} else {
		return act_net, true
	}

	return new_net, false
}


/*
	REVAMP: DEPRECATED
	Given a project id, find the associated gateway.  Returns the whole project/ip string.

func (n *Network) gateway4tid( tid string ) ( *string ) {
	for _, ip := range n.gwmap {				// mac to ip, so we have to look at each value
		toks := strings.SplitN( *ip, "/", 2 )
		if toks[0] == tid {
			return ip
		}
	}

	return nil
}
*/

/*
	Given a host name, generate various bits of information like mac address, switch and switch port.
	Error is set if we cannot find the box.
*/
func (n *Network) host_info( epid *string ) ( ip *string, mac *string, swid *string, swport int, err error ) {
	mac = nil

	if epid == nil {
		err = fmt.Errorf( "cannot translate nil name" )
		return
	}

	ep := n.endpts[*epid]
	if ep == nil {
		return nil, nil, nil, -1, fmt.Errorf( "unable to find endpoint ID: %s", *epid )
	}

	mac, ip = ep.Get_addresses()


	sw, swport := ep.Get_switch_port( )
	if sw != nil {
		swid = sw.Get_id()
	} else {
		err = fmt.Errorf( "cannot generate switch/port for %s", *epid )
		return
	}

	return
}

// --------------------  info exchange/debugging  -----------------------------------------------------------------------------------------

/*
	Request a list of endpoints from openstack.  
	If blocking is requested, then a map of gizmos endpoints is returned, otherwise nil.
*/
func req_ep_list( rch chan *ipc.Chmsg, block bool ) ( map[string]*gizmos.Endpt ) {
	net_sheep.Baa( 2, "requesting ep list from osif" )

	if rch == nil {
		rch = make(  chan *ipc.Chmsg )
	}

	req := ipc.Mk_chmsg( )
	req_str := "_all_proj"
	req.Send_req( osif_ch, rch, REQ_GET_ENDPTS, &req_str, nil )

	if ! block {
		return nil
	}

	req = <- rch							// wait for the response
	if req == nil || req.Response_data == nil {
		net_sheep.Baa( 1, "no data from osif on endpoint request" )
	}
	
	m, ok :=  req.Response_data.( map[string]*gizmos.Endpt )
	if ! ok {
		net_sheep.Baa( 0, "nil end point map returned from osif" )
		return nil
	}

	return m
}

	
/*
	Generate a json list of hosts which includes ip, name, switch(es) and port(s).
*/
func (n *Network) host_list( ) ( jstr string ) {
	var( 	
		sep 	string = ""
	)

	jstr = ` [ `						// an array of objects

	if n != nil && n.endpts != nil {
		for vmid, ep := range n.endpts {
			ip, mac := ep.Get_addresses()
			proj := ep.Get_project()

			sw, port := ep.Get_switch_port( )
			sw_str := &empty_str
			if sw != nil {
				sw_str = sw.Get_id()
			}
			jstr += fmt.Sprintf( `%s { "epid": %q, "mac": %q, "project": %q, "ip": %q, "switch": %q, "port": %d }`, sep, vmid, *mac, *proj, *ip, *sw_str, port )
			sep = ","
		}
	} else {
		net_sheep.Baa( 0, "ERR: host_list: n is nil (%v) or n.hosts is nil  [TGUNET007]", n == nil )
	}

	jstr += ` ]`			// end of hosts array

	return
}

/*
	Generate a json list of fences
*/
func (n *Network) fence_list( ) ( jstr string ) {
	var( 	
		sep 	string = ""
	)

	jstr = ` [ `						// an array of objects

	if n != nil && n.limits != nil {
		for _, f := range n.limits {
			jstr += fmt.Sprintf( "%s%s", sep, f.To_json() )
			sep = ", "
		}
	} else {
		net_sheep.Baa( 0, "limit list is nil, no list generated" )
	}

	jstr += ` ]`			// end of the array

	return
}


/*
	Generate a json representation of the network graph.
*/
func (n *Network) to_json( ) ( jstr string ) {
	var	sep string = ""

	jstr = `{ "netele": [ `

	for k := range n.switches {
		jstr += fmt.Sprintf( "%s%s", sep, n.switches[k].To_json( ) )
		sep = ","
	}

	jstr += "] }"

	return
}

/*
	Transfer maps from an old network graph to this one
*/
func (net *Network) xfer_maps( old_net *Network ) {
	net.limits = old_net.limits
/*
	net.vm2ip = old_net.vm2ip
	net.ip2vm = old_net.ip2vm
	net.vmid2ip = old_net.vmid2ip
	net.ip2vmid = old_net.ip2vmid
	net.vmid2phost = old_net.vmid2phost	
	net.vmip2gw = old_net.vmip2gw
	net.ip2mac = old_net.ip2mac
	net.mac2phost = old_net.mac2phost
	net.gwmap = old_net.gwmap
	net.fip2ip = old_net.fip2ip
	net.ip2fip = old_net.ip2fip
*/
}

/*
	Loads any endpoints that were tucked away in the data cache. Returns a map that can be given to 
	the build process. If a non-nill map is passed in, then the entries are added to that map (allows
	the map we get from openstack to be added to before we build the network).
*/
func load_endpts( umap map[string]*gizmos.Endpt ) ( m map[string]*gizmos.Endpt, err error ) {
	var endpts map[string]*gizmos.Endpt 

	dc := datacache.Mk_dcache( nil, nil )
	if dc == nil {
		return nil, fmt.Errorf( "unable to link to datacache" )
	}

	epl, err := dc.Get_endpt_list()		// get list of endpoint ids
	if epl == nil {
		if err != nil {
			net_sheep.Baa( 1, "error fetching endpoint list from datacache: %s", err )
		} else {
			net_sheep.Baa( 1, "no endpoints listed in datacache" );
		}

		return umap, err
	}

	if umap == nil {						// no map, create one, otherwise just add to it
		endpts = make( map[string]*gizmos.Endpt, len( epl ) )
	} else {
		endpts = umap
	}

	net_sheep.Baa( 1, "building endpoints from %d listed in datacache", len( epl ) )
	for i := range( epl ) {
		epm, err := dc.Get_endpt( epl[i] )
		if err == nil {
			endpts[epl[i]] = gizmos.Endpt_from_map( epm )
			net_sheep.Baa( 2, "adding endpoint from dc: %s",  endpts[epl[i]] )
		} else {
			net_sheep.Baa( 1, "unable to fetch endpoint from datacahce: %s: %s", epl[i], err )
		}
			
	}

	return endpts, nil
}


// --------- public -------------------------------------------------------------------------------------------

/*
	REVAMP:  sdn host is no longer supported.

	to be executed as a go routine.
	nch is the channel we are expected to listen on for api requests etc.
	sdn_host is the host name and port number where the sdn controller is running.
	(for now we assume the sdn-host is a floodlight host and invoke FL_ to build our graph)
*/
func Network_mgr( nch chan *ipc.Chmsg, topo_file *string ) {
	var (
		act_net *Network
		req				*ipc.Chmsg
		limits map[string]*gizmos.Fence		// user link capacity boundaries
		hlist			*string = &empty_str				// host list we'll give to build should we need to build a dummy star topo
		eps_list	map[string]*gizmos.Endpt				// list of enpoints as delivered by openstack (we add to with VM/net changes)
	)

	net_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	net_sheep.Set_prefix( "netmgr" )
	tegu_sheep.Add_child( net_sheep )					// we become a child so that if the master vol is adjusted we'll react too

	limits = make( map[string]*gizmos.Fence )
	cfg := mk_net_cfg( cfg_data )

	if cfg == nil {
		net_sheep.Baa( 0, "CRI: abort: unable to build a config struct for network goroutine" )
		os.Exit( 1 )
	}

	if cfg.def_ul_cap >= 0 {
		s := "default"
		f := gizmos.Mk_fence( &s, cfg.def_ul_cap, 0, 0 )			// the default capacity value used if specific user hasn't been added to the hash
		limits["default"] = f
		v, _ := f.Get_limits()
		net_sheep.Baa( 1, "link capacity limits set to: %d%%", v )
	}

	if topo_file != nil && *topo_file != "" {
		cfg.topo_file = topo_file						// command line override
		net_sheep.Baa( 1, "topofile in config was overridden from command line: %s", *topo_file )
	}
														// enforce some sanity on config file settings
	if cfg.refresh < 15 {
		net_sheep.Baa( 0, "refresh rate in config file (%ds) was too small; set to 15s", cfg.refresh )
		cfg.refresh = 15
	}
	if cfg.max_link_cap <= 0 {
		cfg.max_link_cap = 1024 * 1024 * 1024 * 10							// if not in config file use 10Gbps
	}

	net_sheep.Baa( 1,  "network_mgr thread started: max_link_cap=%d refresh=%d", cfg.max_link_cap, cfg.refresh )

	net_sheep.Baa( 1, "requesting end point list from osif (blocking until it arrives)" )
	eps_list = req_ep_list( nil, true )										// rquest list -- block until we get it
	if len( eps_list ) <= 0 {
		net_sheep.Baa( 1, "end point list received from osif with %d entries scheduling another request", len( eps_list ) )
		req_ep_list( nch, false )										// rquest list response will come back and be processed in the loop below
	} else {
		net_sheep.Baa( 1, "end point list received from osif with %d entries", len( eps_list ) )
		eps_list, _ = load_endpts( eps_list )											// add anything from the datacahce to it
	}

	act_net = build( nil, eps_list, cfg, &empty_str )
	if act_net == nil {
		net_sheep.Baa( 0, "ERR: initial build of network failed -- core dump likely to follow!  [TGUNET011]" )		// this is bad and WILL cause a core dump
	} else {
		net_sheep.Baa( 1, "initial network graph has been built" )
		act_net.limits = limits
		//act_net.Set_relaxed( relaxed )   // NEED?
	}

	tklr.Add_spot( 2, nch, REQ_CHOSTLIST, nil, 1 ) 		 							// tickle once, very soon after starting, to get a host list
	tklr.Add_spot( int64( cfg.refresh * 2 ), nch, REQ_CHOSTLIST, nil, ipc.FOREVER ) // get a host list from openstack now and again
	tklr.Add_spot( int64( cfg.refresh ), nch, REQ_NETUPDATE, nil, ipc.FOREVER )		// add tickle spot to drive rebuild of network

														// set up message listeners for bus events etc
	event_ch := make( chan *msgrtr.Envelope, 1024 )		// channel for message events
	ev_data := &event_handler_data {
		req_chan: nch,
	}
	msgrtr.Register( "endpt", event_ch, ev_data )			// listen for all network and endpoint messages
	msgrtr.Register( "network", event_ch , ev_data )		// these arrive as msgrtr.Events in the main select below.
	
	for {
		select {							// receive data from any channel; ensure serialisation
			case env := <- event_ch:		// message event received 
				event := env.Event
				tokens := strings.Split( event.Event_type, "." )
				switch tokens[0] {
					case "network":
						netev_net( event, env.Ldata )

					case "endpt":
						netev_endpt( event, env.Ldata )

					default: net_sheep.Baa( 1, "ignored: unknown/unrecognised event received: %s", event.Event_type )
				}

			case req = <- nch:
				req.State = nil				// nil state is OK, no error

				net_sheep.Baa( 4, "processing request %d", req.Msg_type )			// sometimes helps to see what we're doing
				switch req.Msg_type {
					case REQ_NOOP:			// just ignore -- acts like a ping if there is a return channel

					case REQ_STATE:			// return state with respect to whether we have enough data to allow reservation requests
											// we allow updates once the initial ep list has size.	
						state := 0
						if act_net != nil && act_net.ep_good {
							state = 1
						}
						net_sheep.Baa( 1, "net-state: state=%d", state )
						req.Response_data = state

					case REQ_HASCAP:						// verify that there is capacity, and return the path, but don't allocate the path
						p, ok := req.Req_data.( *gizmos.Pledge_bw )
						if ok {
							h1, h2, _, _, commence, expiry, bandw_in, bandw_out := p.Get_values( )
							net_sheep.Baa( 1,  "has-capacity request received on channel  %s -> %s", h1, h2 )
							pcount_in, path_list_out, o_cap_trip := act_net.build_paths( h1, h2, commence, expiry,  bandw_out, cfg.find_all_paths, false );
							pcount_out, path_list_in, i_cap_trip := act_net.build_paths( h2, h1, commence, expiry, bandw_in, cfg.find_all_paths, true ); 	// reverse path

							if pcount_out > 0  && pcount_in > 0  {
								path_list := make( []*gizmos.Path, pcount_out + pcount_in )		// combine the lists
								pcount := 0
								for j := 0; j < pcount_out; j++ {
									path_list[pcount] = path_list_out[j]
									pcount++
								}
								for j := 0; j < pcount_in; j++ {	
									path_list[pcount] = path_list_in[j]
								}

								req.Response_data = path_list
								req.State = nil
							} else {
								req.Response_data = nil
								if i_cap_trip {
									req.State = fmt.Errorf( "unable to generate a path: no capacity (h1<-h2)" )		// tedious, but we'll break out direction
								} else {
									if o_cap_trip {
										req.State = fmt.Errorf( "unable to generate a path: no capacity (h1->h2)" )
									} else {
										req.State = fmt.Errorf( "unable to generate a path:  no path" )
									}
								}
							}
						} else {
							net_sheep.Baa( 1, "internal mishap: pledge passed to has capacity wasn't a bw pledge: %s", p )
							req.State = fmt.Errorf( "unable to create reservation in network, internal data corruption." )
						}

					case REQ_BWOW_RESERVE:								// one way bandwidth reservation, nothing really to vet, return a gate block
						// host names are expected to have been vetted (if needed) and translated to project/uuid/address:port

						req.Response_data = nil
						p, ok := req.Req_data.( *gizmos.Pledge_bwow )
						if ok {
							//src, dest := p.Get_hosts( )									// we assume project/epuuid/ip:port
							src, dest, _, dport, _, _ := p.Get_values( )					// we assume project/epuuid/ip:port for src,dest and need a dest port

							if src != nil && dest != nil {
								net_sheep.Baa( 1,  "network: bwow reservation request received: %s -> %s", *src, *dest )

								usr := "nobody"											// default dummy user if not project/host
								sepid := ""
								toks := strings.SplitN( *src, "/", 3 )					// suss out various bits of stuff from names
								if len( toks ) > 1 {
									usr = toks[0]										// the 'user' for queue setting
								}
								if len( toks ) > 2 {
									sepid = toks[1]
								}

								//ipd := ""
								depid := ""
								toks = strings.SplitN( *dest, "/", 3 )
								if len( toks ) > 1 {
									depid = toks[1]
								}
	
								sh := act_net.endpts[sepid]							// suss out endpoints, dest can be nil and that's ok
								dh := act_net.endpts[depid]
								if sh != nil {
									ssw, _ := sh.Get_switch_port( )
									gate := gizmos.Mk_gate( sh, dh, ssw, p.Get_bandwidth(), usr )
									if (*dest)[0:1] == "!" || dh == nil {								// indicate that dest IP cannot be converted to a MAC address
										gate.Set_extip( dest )
									} else {									
										if *dport != "0" {							// must check for port, and if there must set external address
											gate.Set_extip( dest )
										}
									}

									c, e := p.Get_window( )												// commence/expiry times
									fence := act_net.get_fence( &usr )
									max := int64( -1 )
									if fence != nil {
										max = fence.Get_limit_max()
									}
									if gate.Has_capacity( c, e, p.Get_bandwidth(), &usr, max ) {		// verify that there is room
										qid := p.Get_id()												// for now, the queue id is just the reservation id, so fetch
										p.Set_qid( qid ) 												// and add the queue id to the pledge
	
										if gate.Add_queue( c, e, p.Get_bandwidth(), qid, fence ) {		// create queue AND inc utilisation on the link
											req.Response_data = gate									// finally safe to set gate as the return data
											req.State = nil												// and nil state to indicate OK
										} else {
											net_sheep.Baa( 1, "owreserve: internal mishap: unable to set queue for gate: %s", gate )
											req.State = fmt.Errorf( "unable to create oneway reservation: unable to setup queue" )
										}
									} else {
										net_sheep.Baa( 1, "owreserve: switch does not have enough capacity for a oneway reservation of %s", p.Get_bandwidth() )
										req.State = fmt.Errorf( "unable to create oneway reservation for %d: no capacity on (v)switch: %s", p.Get_bandwidth(), gate.Get_sw_name() )
									}
								} else {
									net_sheep.Baa( 1, "owreserve: unable to parse one of the endpoint strings: %s %s", src, dest )
									req.State = fmt.Errorf( "unable to create oneway reservation for %d: unable to parse endpoint name(s)" )
								}
							} else {
								net_sheep.Baa( 1, "owreserve: one/both host names were invalid" )
								req.State = fmt.Errorf( "unable to create oneway reservation in network one or both host names invalid" )
							}
						} else {									// pledge wasn't a bw pledge
							net_sheep.Baa( 1, "internal mishap: pledge passed to owreserve wasn't a bwow pledge: %s", p )
							req.State = fmt.Errorf( "unable to create oneway reservation in network, internal data corruption." )
						}

					case REQ_BW_RESERVE:
						//var ip2		*string = nil					// tmp pointer for this block

						// host names are expected to have been vetted (if needed) and translated to project/uuid/address
						p, ok := req.Req_data.( *gizmos.Pledge_bw )
						if ok {
							h1, h2, _, _, commence, expiry, bandw_in, bandw_out := p.Get_values( )		// ports can be ignored
							net_sheep.Baa( 1,  "network: bw reservation request received: %s -> %s  from %d to %d", *h1, *h2, commence, expiry )

							suffix := "bps"
							if cfg.discount > 0 {
								if cfg.discount < 101 {
									bandw_in -=  ((bandw_in * cfg.discount)/100)
									bandw_out -=  ((bandw_out * cfg.discount)/100)
									suffix = "%"
								} else {
									bandw_in -= cfg.discount
									bandw_out -= cfg.discount
								}

								if bandw_out < 10 {			// add some sanity, and keep it from going too low
									bandw_out = 10
								}
								if bandw_in < 10 {
									bandw_in = 10
								}
								net_sheep.Baa( 1, "bandwidth was reduced by a discount of %d%s: in=%d out=%d", cfg.discount, suffix, bandw_in, bandw_out )
							}

							ep1_name := act_net.defrock_epname( h1 )			// endpoint uuids
							ep2_name := act_net.defrock_epname( h2 )

							if ep1_name != "" && ep2_name != "" {
								net_sheep.Baa( 2,  "network: attempt to find outbound path between  %s -> %s", ep1_name, ep2_name )
								pcount_out, path_list_out, o_cap_trip := act_net.build_paths( &ep1_name, &ep2_name, commence, expiry, bandw_out, cfg.find_all_paths, false ); 	// outbound path
								net_sheep.Baa( 2,  "network: attempt to find inbound  path between  %s -> %s", ep1_name, ep2_name )
								pcount_in, path_list_in, i_cap_trip := act_net.build_paths( &ep2_name, &ep1_name, commence, expiry, bandw_in, cfg.find_all_paths, true ); 		// inbound path

								if pcount_out > 0  &&  pcount_in > 0  {
									net_sheep.Baa( 1,  "network: %d acceptable path(s) found icap=%v ocap=%v", pcount_out + pcount_in, i_cap_trip, o_cap_trip )

									path_list := make( []*gizmos.Path, pcount_out + pcount_in )		// combine the lists
									pcount := 0
									for j := 0; j < pcount_out; j++ {
										path_list[pcount] = path_list_out[j]
										pcount++
									}
									for j := 0; j < pcount_in; j++ {	
										path_list[pcount] = path_list_in[j]
										pcount++
									}

									qid := p.Get_id()											// for now, the queue id is just the reservation id, so fetch
									p.Set_qid( qid )											// and add the queue id to the pledge

									for i := 0; i < pcount; i++ {								// set the queues for each path in the list (multiple paths if network is disjoint)
										fence := act_net.get_fence( path_list[i].Get_usr() )
										net_sheep.Baa( 2,  "\tpath_list[%d]: %s -> %s  (%s)", i, *h1, *h2, path_list[i].To_str( ) )
										path_list[i].Set_queue( qid, commence, expiry, path_list[i].Get_bandwidth(), fence )		// create queue AND inc utilisation on the link
										if cfg.mlag_paths {
											net_sheep.Baa( 1, "increasing usage for mlag members" )
											path_list[i].Inc_mlag( commence, expiry, path_list[i].Get_bandwidth(), fence, act_net.mlags )
										}
									}

									req.Response_data = path_list
									req.State = nil
								} else {
									req.Response_data = nil
									if i_cap_trip {
										req.State = fmt.Errorf( "unable to generate a path: no capacity (h1<-h2)" )		// tedious, but we'll break out direction
									} else {
										if o_cap_trip {
											req.State = fmt.Errorf( "unable to generate a path: no capacity (h1->h2)" )
										} else {
											req.State = fmt.Errorf( "unable to generate a path:  no path" )
										}
									}
									net_sheep.Baa( 0,  "no paths in list: %s  cap=%v/%v", req.State, i_cap_trip, o_cap_trip )
								}
							} else {
								net_sheep.Baa( 0,  "network: unable to map to an enpoint uuid: %s (%s) %s (%s)",  *h1, ep1_name, *h2, ep2_name )
								req.State = fmt.Errorf( "one of the endpoint uuids is not known: %s %s", *h1, *h2 )
							}
						} else {									// pledge wasn't a bw pledge
							net_sheep.Baa( 1, "internal mishap: pledge passed to reserve wasn't a bw pledge: %s", p )
							req.State = fmt.Errorf( "unable to create reservation in network, internal data corruption." )
						}



					case REQ_DEL:									// delete the utilisation for the given reservation
						switch p := req.Req_data.( type ) {
							case *gizmos.Pledge_bw:
								net_sheep.Baa( 1,  "network: deleting bandwidth reservation: %s", *p.Get_id() )
								commence, expiry := p.Get_window( )
								path_list := p.Get_path_list( )
		
								qid := p.Get_qid()							// get the queue ID associated with the pledge
								for i := range path_list {
									fence := act_net.get_fence( path_list[i].Get_usr() )
									net_sheep.Baa( 1,  "network: deleting path %d associated with usr=%s", i, *fence.Name )
									path_list[i].Set_queue( qid, commence, expiry, -path_list[i].Get_bandwidth(), fence )		// reduce queues on the path as needed
								}

							case *gizmos.Pledge_bwow:
								net_sheep.Baa( 1,  "network: deleting oneway reservation: %s", *p.Get_id() )
								commence, expiry := p.Get_window( )
								gate := p.Get_gate()
								fence := act_net.get_fence( gate.Get_usr() )
								gate.Set_queue( p.Get_qid(), commence, expiry, -p.Get_bandwidth(), fence )				// reduce queues

							default:
								net_sheep.Baa( 1, "internal mishap: req_del wasn't passed a bandwidth or oneway pledge; nothing done by network" )
							
						}

					// should be deprecated with new_ep 
					case REQ_ADD:							// insert new information into the various vm maps
						net_sheep.Baa( 1, "##### deprecated REQ_ADD sent to network manager" )
						/* --- deprecated
						if req.Req_data != nil {
							switch req.Req_data.( type ) {
								case *Net_vm:
									vm := req.Req_data.( *Net_vm )
									act_net.insert_vm( vm )

								case []*Net_vm:
									vlist := req.Req_data.( []*Net_vm )
									for i := range vlist {
										act_net.insert_vm( vlist[i] )
									}
							}

							new_net := build( act_net, sdn_host, max_link_cap, link_headroom, link_alarm_thresh, hlist )
							if new_net != nil {
								new_net.xfer_maps( act_net )				// copy maps from old net to the new graph
								act_net = new_net							// and finally use it
							}
						}
						--- */

											//----------------- end point management ------------------------
					case REQ_EP2MAC:										// given an endpoint name return the mac address
						if req.Response_ch != nil {
							var ep *gizmos.Endpt
							var ep_uuid string

							switch epid := req.Req_data.( type ) {
								case string:
									ep = act_net.endpts[epid]
									ep_uuid = epid

								case *string:
									ep = act_net.endpts[*epid]
									ep_uuid = *epid
								
							}

							if ep != nil {
								req.Response_data = *(ep.Get_mac())
							} else {
								net_sheep.Baa( 2, "ep2mac did not map to a known enpoint: %s", ep_uuid )
								req.Response_data = ""
							}
							//net_sheep.Baa( 1, ">>>> xlating ep2mac responding: %s", req.Response_data.( string ) )
						}

					case REQ_EP2PROJ:										// given an endpoint name return the project id
						if req.Response_ch != nil {
							var ep *gizmos.Endpt

							switch epid := req.Req_data.( type ) {
								case string:
									ep = act_net.endpts[epid]

								case *string:
									ep = act_net.endpts[*epid]
							}

							if ep != nil {
								req.Response_data = *(ep.Get_meta_value( "project" ))
							} else {
								req.Response_data = ""
							}
						}

					case REQ_NEW_ENDPT:										// add a new endpoint, or endpoint set to the graph
						req.Response_ch = nil								// ensure we don't try to write back on this
						if req == nil || req.Req_data == nil {
							net_sheep.Baa( 1, "no data on new endpoint reqeust" )
						} else {
							if m, ok := req.Req_data.( map[string]*gizmos.Endpt ); ok {		// pick up the map if it is the expected type
								act_net, _ = update_net( act_net, m, cfg, hlist )
							} else {
								net_sheep.Baa( 1, "new endpoint reqeust contained bad data" )
							}
						}

					case REQ_GET_ENDPTS:									// response back from osif if we had to make additional requests (os down?)
						req.Response_ch = nil								// ensure we don't try to write back on this
						req_again := true									// assume we'll need to request it again
						if req == nil || req.Response_data == nil {
							net_sheep.Baa( 1, "no data on endpoint response" )
						} else {
							if m, ok := req.Response_data.( map[string]*gizmos.Endpt ); ok {		// pick up the map if it is the expected type
								if len( m ) > 0 {
									load_endpts( m )												// add any from the cache
									act_net, req_again = update_net( act_net, m, cfg, hlist )
								} else {
									req_again = false
								}
							} else {
								net_sheep.Baa( 1, "response from ostack endpoint request was not expected map type" )
							}
						}

						if req_again {
							req_ep_list( nch, false )					// request another which will process here when received
						}

					case REQ_GEN_QMAP:							// generate a new queue setting map
						ts := req.Req_data.( int64 )			// time stamp for generation
						req.Response_data, req.State = act_net.gen_queue_map( ts, false )

					case REQ_GEN_EPQMAP:						// generate a new queue setting map but only for endpoints
						ts := req.Req_data.( int64 )			// time stamp for generation
						req.Response_data, req.State = act_net.gen_queue_map( ts, true )
						

					case REQ_GETPHOST:							// given a name or IP address, return the physical host
						net_sheep.Baa( 1, "deprecated REQ_ADD sent to network manager" )
					/* --- deprecated -- unused and with endpoint not needed
						if req.Req_data != nil {
							var ip *string

							s := req.Req_data.( *string )
							ip, req.State = act_net.name2ip( s )
							if req.State == nil {
								req.Response_data = act_net.mac2phost[*act_net.ip2mac[*ip]]
								if req.Response_data == nil {
									req.State = fmt.Errorf( "cannot translate IP to physical host: %s", ip )
								}	
							}
						} else {
							req.State = fmt.Errorf( "no data passed on request channel" )
						}
					------ */
						
					case REQ_GETIP:								// given an endpoint uuid return the default (first) ip address
						if req.Req_data != nil {
							s := req.Req_data.( *string )
							req.Response_data, req.State = act_net.epid2ip( s )		// returns ip or nil
						} else {
							req.State = fmt.Errorf( "no data passed on request channel" )
						}
					
					//REVAMP:  this is used only by old steering and may be deprecated
					case REQ_HOSTINFO:							// generate a string with mac, ip, switch-id and switch port for the given host
						if req.Req_data != nil {
							ip, mac, swid, port, err := act_net.host_info(  req.Req_data.( *string ) )
							if err != nil {
								req.State = err
								req.Response_data = nil
							} else {
								req.Response_data = fmt.Sprintf( "%s,%s,%s,%d", *ip, *mac, *swid, port )
							}
						} else {
							req.State = fmt.Errorf( "no data passed on request channel" )
						}

					case REQ_GETLMAX:							// DEPRECATED!  request for the max link allocation
						req.Response_data = nil;
						req.State = nil;

					case REQ_NETUPDATE:											// build a new network graph
						net_sheep.Baa( 2, "rebuilding network graph" )			// less chatty with lazy changes
						new_net := build( act_net, nil, cfg, hlist )
						if new_net != nil {
							new_net.xfer_maps( act_net )						// copy maps from old net to the new graph
							act_net = new_net

							net_sheep.Baa( 2, "network graph rebuild completed" )		// timing during debugging
						} else {
							net_sheep.Baa( 1, "unable to update network graph -- SDNC down?" )
						}


					case REQ_CHOSTLIST:								// this is tricky as it comes from tickler as a request, and from osifmgr as a response, be careful!
																	// this is similar, yet different, than the code in fq_mgr (we don't need phost suffix here)
						req.Response_ch = nil;						// regardless of source, we should not reply to this request

						if req.State != nil || req.Response_data != nil {				// response from ostack if with list or error
							if  req.Response_data.( *string ) != nil {
								hls := strings.TrimLeft( *(req.Response_data.( *string )), " \t" )		// ditch leading whitespace
								hl := &hls
								if *hl != ""  {
									hlist = hl										// ok to use it
									net_sheep.Baa( 2, "host list received from osif: %s", *hlist )
								} else {
									net_sheep.Baa( 1, "empty host list received from osif was discarded" )
								}
							} else {
								net_sheep.Baa( 0, "WRN: no  data from openstack; expected host list string  [TGUFQM009]" )
							}
						} else {
							req_hosts( nch, net_sheep )					// send requests to osif for data
						}
								
					//	------------------ user api things ---------------------------------------------------------
					case REQ_SETULCAP:							// user link capacity; expect array of two string pointers
						data := req.Req_data.( []*string )
						val := clike.Atoi64( *data[1] )	
						if val < 0 {							// drop the user fence
							delete( act_net.limits, *data[0] )
							net_sheep.Baa( 1, "user link capacity deleted: %s", *data[0] )
						} else {
							f := gizmos.Mk_fence( data[0], val, 0, 0 )			// get the default frame
							act_net.limits[*data[0]] = f
							net_sheep.Baa( 1, "user link capacity set: %s now %d%%", *data[0], f.Get_limit_max() )
						}
						
					case REQ_NETGRAPH:							// dump the current network graph
						req.Response_data = act_net.to_json()
						// TESTING -- remove next line before flight
						req_ep_list( nch, false )										// rquest list -- block until we get it

					case REQ_LISTHOSTS:							// spew out a json list of hosts with name, ip, switch id and port
						req.Response_data = act_net.host_list( )

					case REQ_LISTULCAP:							// user link capacity list
						req.Response_data = act_net.fence_list( )

					case REQ_LISTCONNS:							// for a given endpoint spit out the switch and port it is connected to
						epid, ok := req.Req_data.( *string )
						req.Response_data = nil			// assume failure
						if ok && epid != nil {
							ep := act_net.endpts[*epid]
							if ep != nil {
								req.Response_data = ep.To_json( );
							} else {
								req.State = fmt.Errorf( "did not find endpoint: %s", *epid )
							}
						} else {
							req.State = fmt.Errorf( "internal mishap: bad data passed to network manager list conns" )
						}

					case REQ_GET_PHOST_FROM_MAC:			// try to map a MAC to a phost -- used for mirroring
						mac, ok := req.Req_data.( *string )
						if ok {
							for _, ep := range act_net.endpts {
								if ep.Equals_meta( "mac", mac ) {
									req.Response_data = ep.Get_meta_value( "phost" )
								}
							}
						}
						/*
						for k, v := range act_net.mac2phost {
							if *mac == k {
								req.Response_data = v
							}
						}
						*/

					case REQ_SETDISC:
						req.State = nil;	
						req.Response_data = "";			// we shouldn't send anything back, but if caller gave a channel, be successful
						if req.Req_data != nil {
							d := clike.Atoll( *(req.Req_data.( *string )) )
							if d < 0 {
								cfg.discount = 0
							} else {
								cfg.discount = d
							}
						}

					// --------------------- agent things -------------------------------------------------------------
					case REQ_MAC2PHOST:
						req.Response_ch = nil			// we don't respond to these
						net_sheep.Baa( 1, "mac2phost list from agent ignored" )


					default:
						net_sheep.Baa( 1,  "unknown request received on channel: %d", req.Msg_type )
				}

				net_sheep.Baa( 4, "processing request complete %d", req.Msg_type )
				if req.Response_ch != nil {				// if response needed; send the request (updated) back
					req.Response_ch <- req
				}

		}
	}
}
