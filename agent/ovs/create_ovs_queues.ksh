#!/usr/bin/env ksh
# vim: sw=4 ts=4:

#	Mnemonic:	create_ovs_queues.ksh
#	Abstract:	This reads a list of switch/port/queue data generated by tegu and 
#				creates the OVS command to create the necessary individual queues, 
#				the queue combinations (QoS entries in OVS terms) and the mappings
#				of switch/port to the proper queue combinations. The mehod that OVS
#				provides to set a simple switch/port/queue is maddening (WARNING: tinkering
#				with this script might put your sanity at serious risk.)
#
#				OVS manages indivual queues, and groups them together into combinations
#				that it calls QoS.  A port may then be assigned a combination which 
#				then defines the queues for that port.  It's a bloody lot of hoops 
#				to jump through just to say that switch A, port n has queues 1,2,3. 
#
#				The logic here at a highlevel is:
#					Dump a list of switch/port information from ovs in order to map
#					each port to a uuid. 
#
#					Read the Tegu generated switch/port/queue requirements
#
#					Generate the command that defines the mappings. This is a series
#					of indivual queue defintions, the groupings of the defintions
#					into the needed combinations, and finally the assignment of a 
#					combination to each switch/port as defined in the Tegu data. 
#
#				The tegu data is: switch/port,resname,queue,min,max,priority. The 
#				switch and port might be set for late binding which means this has to 
#				do more work.  If switch matches the host name of the host we're working
#				on (-h), then the assumption is that the switch is br-int and that
#				port is either a mac address of the VM or a designation that can be used
# 				to map to a port away from the vm. Currently that designation is a constant
#				-128, but someday should change to something like @em1 which would be 
#				used to find int-br-em1 rather than assuming the first outward facing 
#				port that we encounter in the list.
#
#				The tegu data is expected to reside in a file which is named on the command 
#				line and that the script is expected to delete after processing.  Tegu does
#				NOT set values for queue 0.  The values for queue 0 are computed by this
#				script and are given a priority of 1000 (lower than what we expect tegu
#				to set as a priority).
#
#				We've hacked our original source to allow it to be run from a centeralised
#				site such that only a login on the remote site exists (none of our software
#				is installed). To do this we bundle as many ovs-* commands into a single ssh
#				command that is executed on the remote host rather than an ssh command for
#				each.  
#
#				Further hacking to support VLAN trunking from a VM...
#				The requirement is to put all traffic from a VM which has a reservation onto
#				br-rl. We now set up an inbound queue on br-rl's interface when the port is -128
#				using the queue number given.  All inbound traffic is expected to be put on 
#				queue 1 for priority treatment, but no cap is enforced because it's assumed to 
#				have been rate limited at ingress.
#
#				There is also now the need to allow a queue number to be supplied for a switch
#				multiple times, and for the max value to be the sum of each specification. This 
#				is a requirement because if engress queues are turned off, what would be the 
#				egress queue for the VM is turned into the queue on br-rl which may be a dup.
#				There isn't a way to avoid the dup without really hacking Tegu, and since the
#				hope is that ratelimting will eventually be built into OVS, the hack will be 
#				here. 
#
#	Author:		E. Scott Daniels
#	Date: 		10 November 2013
#
#	Mods:		23 Jan 2013 - enhanced the expand function to suport K, KiB and k style suffixes
#					and added a cap to the limit set on q1 traffic.
#				20 Feb 2013 - Major revision to allow individual queues for each reservation to be set.
#				11 Mar 2014 - Corrected problem caused by inserting sudo in front of a command.
#				20 Mar 2014 - Now pulls specific fields from sp2uuid output rather than assuming that
#					the last field is the ID needed (allows sp2uuid to output additional info in future).
#				23 Apr 2014 - Hacks to allow it to be run centrally. (This is NOT efficent, but I don't
#					think purging will be common, so for now we'll leave it with the small, and easy, hack.)
#				01 May 2014 - Changes to do the late binding of router and port info.
#				03 May 2014 - Added limit option to provide scope on which OVS bridges are affected (cleared).
#				04 May 2014 - Corrected bug that was puttting out too many combo queue commands.
#				12 May 2014 - Added code to support cases where there are multiple interfaces. This is only
#					a HACK at the moment. The host ID when port is -128 (late binding) needs to include
#					the attachement point (e.g. qos106@eth1) so that we can set a queue on JUST that 
#					interface, and not all outward facing interfaces on brint. 
#				13 May 2014 - Added ssh options to prevent prompts when new host tried
#				04 Oct 2014 - Correct check that was stumbling over the extra information that ovs_sp2uuid
#					is now emitting.
#				05 Oct 2014 - Better error messages.
#				05 Nov 2014 - Added burst settings to the queues to prevent packet loss specifically
#					in br-int on the receiver side. (bug tracker #245)
#				10 Nov 2014 - Set connect timeout for ssh calls to 10s
#				17 Nov 2014 - Set burst on best effort queues too.
#					Added timeouts on ssh commands to prevent "stalls" as were observed in pdk1.
#				01 Dec 2014 - Change to allow set intermediate queue to use uuid rather than the switch's
#					mac address.
#				04 Dec 2014 - Ensured that all crit/warn messages have a constant target host component.
#				05 Dec 2014 - Corrected bug with picking up uuid for switch id rather than MAC.
#				16 Dec 2014 - Corrected bug caused because we must use uuids when doing intermediate queues
#					but tegu doesn't know anything but mac addresses.
#				28 Jan 2014 - Checks to see that -h isn't the current host or 'localhost'; sets ssh exec 
#					only if different.
#				24 Mar 2015 - More hacking to make this work for the VLAN trunking changes that push
#					all reservation traffic over br-rl and thus all -128 queues should be set on br-rl's
#					qosirl0 interface. To summarise:
#						- static queue0 is on by default, -S turns it off
#						- queues are no longer written to egress VMs unless -e is set
#						- outward queues (port == -128) are written only to qosirl interfaces unless -A is set
#				08 Apr 2015 - Corrected typos in error messages, must call ql_set_trunks to set up trunks on 
#					the qosirl0 interface.
#				07 Map 2015 - Corrected bug on ql_set_trunks call (set_trukns was the bug). It appears that 
#					setting trunks isn't needed, so it's been commented out at the moment.
# ----------------------------------------------------------------------------------------------------------
#
#  Some OVS QoS and Queue notes....
# 	the ovs queue is a strange beast with confusing implementation. There is a QoS 'table' which defines 
# 	one or more entries with each entry consisting of a set of queues.  Each port can be assigned one QoS 
# 	entry which serves to define the queue(s) for the port.  
# 	An odd point about the QoS entry is that the entry itself caries a max-rate setting (no min-rate), but
# 	it's not clear in any documentation as to how this is applied.  It could be a default applied to all 
# 	queues such that only the min-rate for the queue need be set, or it could be used as a hard max-rate
# 	limit for all queues on the port in combination. 
#
# 	Further, it seems to cause the switch some serious heartburn if the controller sends a flowmod to the 
# 	switch which references a non-existant queue, this in turn causes some serious stack dumping in the
# 	controller.


# ----------------------------------------------------------------------------------------------------------

trap "cleanup" 1 2 3 15 EXIT
function cleanup 
{
	rm -f /tmp/PID$$.*
}

#
# expand a value suffixed with G, GiB or g into a 'full' value (XiB or x are powers of 2, while X is powers of 10)
function expand
{
	case $1 in 
		*KiB)		echo $(( ${1%K*} * 1024 ));;
		*k)			echo $(( ${1%k*} * 1024 ));;
		*K)			echo $(( ${1%?} * 1000 ));;

		*MiB)		echo $(( ${1%M*} * 1024 * 1024 ));;
		*m)			echo $(( ${1%m*} * 1024 * 1024 ));;
		*M)			echo $(( ${1%?} * 1000000 ));;

		*GiB)		echo $(( ${1%G*} * 1024 * 1024 * 1024 ));;
		*g)			echo $(( ${1%g*} * 1024 * 1024 * 1024 ));;
		*G)			echo $(( ${1%?} * 1000000000 ));;

		*)			echo $1;;
	esac
}

function logit
{
	echo "$(date "+%s %Y/%m/%d %H:%M:%S") $argv0: $@" >&2
}

function usage
{
	cat <<-endKat


	version 2.2/15064
	usage: $argv0 [-d] [-E] [-e entry-max-rate] [-g] [-h host] [-k] [-l brname] [-n] [-s] [-v] tegu-data-filename

	The tegu data file is the list of swtich, port, queue information that is generated by 
	tegu and is the basis for determining which queues are to be configured. 

	-d delete the data file after reading it
	-e defines the overarching max rate that is assigned to the QoS set. This defaults to 10Gbps.
	-E enables egress queues to be established.  this is a queue on the VM's qvo interface to br-int.
	-g disable the generation of the generic q1 created on all outward interfaces
	-h causes queues to be created on the named host
	-k keep existing queues (do not purge unreferenced queues from OVS)
	-l defines the OVS bridges that will be affected (defaults to br-int). May be supplied multiple times
	   or multiple bridge names may be supplied with spaces separating them.
	   use "all" to affect all bridges without naming them individually. 
	-n is no execute mode 
	-s disable static queue 0 sizing. (implies -g)
	-v verbose mode (dumps the OVS queue setting commands)

	endKat
	
	exit 1
}

# With the deprecation of endpoint (egress) queues, there is a small chance that there will be a queue
# duplication on an endpoint switch.  Because Tegu is forcing same switch reservations to use Q1 on 
# the rate limiting bridge we don't expect this to be needed, but on the off chance that a queue is 
# duplicated, we must ensure it is set with the sum of the values. 
#
# This function parses the tegu queue data, prefixing each output records with "data: " and eliminating
# duplicates.
#
function preprocess_data
{
	awk -F , '
		{
			if( $4 != $5 )			# min and max _should_ be the same, but if not pick the smallest we see
			{
				if( min[$1" "$3] == 0  ||  $4 < min[$1" "$3] )
					min[$1" "$3] = $4
			} else {
				min[$1" "$3] += $4
			}
				
			max[$1" "$3] += $5
			rid[$1" "$3] = $2
			pri[$1" "$3] = $6

			next;
		}

		END {
			for( x in max ) {
				split( x, a, " " )
				printf( "data: %s,%s,%s,%s,%s,%s\n", a[1], rid[x], a[2], min[x], max[x], pri[x] )
			}
		}
	' $1
}

# --------------------------------------------------------------------------------------------------------------

argv0=${0##*/}

if [[ $argv0 == "/"* ]]
then
	PATH="$PATH:${argv0%/*}"		# ensure the directory that contains us is in the path
fi

one_gbit=1000000000

entry_max_rate=$(expand 10G)
forreal=1
purge_ok=1

verbose=0
forreal=1
delete_data=0
						# both host values set when -h given on the command line
rhost=""				# given on commands like ovs_sp2uuid (will be -h foo)
ssh_host=""				# when -h given, sets so that commands are executed on host
ssh_opts="-o ConnectTimeout=10 -o StrictHostKeyChecking=no -o PreferredAuthentications=publickey"
thost=$(hostname)		# target host defaults here, but overridden by -h
limit=""				# limit to just the named switch(es) (-l) (default set on awk call NOT here)
log_file=""
static_q0size=1			# -S disables; causes q0 to be statically sized rather than variable based on other queue sizes
egress_queue=0			# set a queue into (egress) the VM (off by default, -E sets it so we create one)
all_outward_ports=0		# -A and -l enables this; if off, then we set queues only on br-rl's qosirl0 port
gen_q1=1				# -g disables; generate a 'synthetic' q1 for outward queues assuming setup intermediate queues is disabled
noexec_arg=""			# set to -n by -n to pass to various things

while [[ $1 == -* ]]
do
	case $1 in 
		-A)	all_outward_ports=1;;
		-D)	;;							# ignore deprecated 
		-d)	delete_data=1;;
		-e)	entry_max_rate=$( expand $2 ); shift;;
		-E)	egress_queue=1;;
		-g)	gen_q1=0;;
		-h)	
			if [[ $2 != $thost && $2 != "localhost" ]]			# set for ssh if -h is a differnt host
			then
				thost=$2; 						# override target host
				rhost="-h $2"; 					# set option for any ovs_sp2uuid calls etc
				ssh_host="ssh $ssh_opts $2";  	# ssh command for any ovs-* command bundles
			fi
			shift
			;;

		-k)	purge_ok=0;;
		-l) all_outward_ports=1; limit+="$2 "; shift;;		# implies all outward ports
		-L)	log_file=$2; shift;;
		-n)	purge_ok=0; delete_data=0; forreal=0; noexec_arg="-n";;
		-s)	static_q0size=1;;					# now the default, but backward compatable
		-S) static_q0size=0;;
		-v) verbose=1;;

		-\?)	usage
				exit 1
				;;

		*)	echo "unrecognised option: $1" >&2
			usage
			exit 1
			;;
	esac
	shift
done

if [[ -n $log_file ]]			# helps to capture output when executed by agent
then
	exec  2>$log_file
fi

cmd_file="/tmp/PID$$.cmds"
echo "set -e" >$cmd_file		# initialise remote cmd bundle forcing it to exit immediately on error

if (( $(id -u) != 0 ))
then
	sudo="sudo"					# must use sudo for the ovs-vsctl commands
fi

# it doesn't seem that it is necessary (any longer) to list all vlan IDs on a trunk, so 
# for now it's disabled.  Keep it here because if we need to have it this is where it belongs.
#ql_set_trunks $noexec_arg		# must set trunks on the qos-irl interface

if [[ -z $1 ]]			# assume data on stdin
then
	delete_data=0
	data_file=""
	if (( verbose ))
	then
		logit "reading from standard in  [OK]" 
	fi
else
	if [[ $1 != "/dev/null"  &&  ! -f $1 ]]		# dev/null doesn't test as a file for some reason
	then
		logit "first parameter ($1) is not a file  [FAIL]" 
		ls -al $1 >&2 2>/dev/null
		exit 1
	fi

	data_file=$1
fi

create_list=""
(											# order here is IMPORTANT as awk must have switch list first
	if ! ovs_sp2uuid -a $rhost any				# list each switch on the host and a uuid to port mapping for each switch/port combination
	then
		echo "ERROR!"
	fi
	if (( gen_q1 ))							# synthetic q1 for br-rl assuming that setup intermediate queues is not generating this
	then
		echo "data: $thost/-128,priority-in,1,10000000000,10000000000,200"
	fi

	preprocess_data $data_file				# must preprocess to combine queues for the vlinks
	#if ! sed 's/^/data: /' $data_file
	#then
	#	echo "ERROR!"
	#fi
) | awk \
	-v static_q0size="${static_q0size:-1}" \
	-v limit_lst="${limit:-br-int}" \
	-v thost="${thost%%.*}" \
	-v sudo="$sudo" \
	-v max_rate=$entry_max_rate \
	-v aop=${all_outward_ports:-0} \
	-v egress_queues=${egress_queues:0} \
	'
	BEGIN {
		qlsep = "";
		
		n = split( limit_lst, a, " " )	# create hash to define what we set/clear
		for( i = 1; i <= n; i ++ )
			limit[a[i]] = 1

		#burst_string = "other-config:burst=250000b other-config:cburst=250000b other-config:quantum=9000"; 		# quantum nice to have, but ignored by ovs
		burst_string = "other-config:burst=250000b other-config:cburst=250000b"; 		# larger burst settings for queues
		qtype="type=linux-htb"						# might someday provide alternates, so make this a variable
	}
	/ERROR!/ { print "ERROR!"; exit( 1 ) }				# something not right generating ovs data

	/^switch: / && NF > 2 { 				# collect switch data
		if( limit["all"] || limit[$2] || limit[$4] )		# if switch dpid or name is in the list, or allowing all
		{
			swmap[$2] = $3;					# map of switches that live on the host (by mac)
			swmap[$3] = $3;					# setup intermediate queues now lists things by uuid, so map by uuid too
			cur_switch = $3;				# current switch, all ports that follow are childern of this
			cur_swmac = $2;					# also need to track switch by its mac because tegu only has mac addresses
			alt2switch[$4] = $2				# pick up br-int etc to do late binding; map to switch id in ovs
		}
		else
		{
			cur_switch = ""
			cur_swmac = ""
		}
		next;
	}

	/^port: / && NF > 1 {					# collect port data allowing us to map port/queue to a uuid
		if( cur_switch == "" )				# not tracking this switch
			next;

		swpt2uuid[cur_switch,$3] = $2;		# map switch/port combination to uuid
		swpt2uuid[cur_swmac,$3] = $2;

		q0max[cur_switch,$3] = max_rate;	# q0 on switch/port starts at full bandwidth
		q0max[cur_swmac,$3] = max_rate;

		assigned[cur_switch"-"$3] = -1;		# track what we assigned so we can delete everything else
		assigned[cur_swmac"-"$3] = -1;

		if( NF > 5 )						# add mac to allow mac to port mapping for late binding (2014.10.04)
		{
			xmac = $5;
			gsub( ":", "", xmac );
			mac2port[cur_switch,xmac] = $3				# needed to do late binding of switch to vm ports
			mac2port[cur_swmac,xmac] = $3
		}
		else								# assume this is something like int-br-em* (no mac or additional information available)
		{
			if( alt2switch[$4] == "" )		# there is an "internal port" which maps to the switch name; ignore that and capture all others
			{
				if( aop || substr( $4, 1, 6 )  == "qosirl" )						# mapping to all outward ports, or this is the br-rl interface
				{
					sw2sw_links[cur_switch] = sw2sw_links[cur_switch] $4 " "		# collect all switch to switch link names (e.g. int-br-eth2)
					sw2sw_links[cur_swmac] = sw2sw_links[cur_swmac] $4 " "
	
					sw2sw_ports[cur_switch] = sw2sw_ports[cur_switch] $3 " " 		# collect all switch to switch port numberss
					sw2sw_ports[cur_swmac] = sw2sw_ports[cur_swmac] $3 " "
	
					swlinks2port[cur_switch,$4] = $3								# this captures the name which we will use if we add ability to map to @br-eth3 or somesuch
					swlinks2port[cur_swmac,$4] = $3
				}
			}
		}
		next;
	}

	/data: / {							# tegu data: switch/port,res-name,qnum,min,max,priority
		gsub( ":", "", $0 );
		split( $2, a, "," );			# pick apart the parms
		split( a[1], b, "/" );			# sep switch id from port

		sw = b[1];						# convenience vars; switch/port and queue
		pt = b[2];
		spq = a[3]+0;

										# embarrassing hack for q-lite :(
		if( sw == thost )				# if this is the host we are working for, then it imples br-int and we will interpret port accordingly
		{								# if its a host, but not ours, we leave it alone as that will have the same effect as a switch in the list that isnt here
			sw = alt2switch["br-int"]				# translate to mac

			if( pt != "-128" )			# port is a mac; implies inward toward vm port (-128 is handled later)
			{
				pt = mac2port[sw,pt];
			}
		}

		if( !(sw in swmap) )			# switch not on this host
			next;

		mmp = sprintf( "%d-%d-%d", a[4], a[5], a[6] )		# min/max/priority (a unique set of queue parms, generate the queue creation goo
		if( !(mmp in iq) )
		{
			qlist = qlist qlsep sprintf( "%d=@iq%d", qid, qid );		# individual queue definition
			qlsep=","
			iq[mmp] = sprintf( "-- --id=@iq%d create Queue other-config:min-rate=%.0f other-config:max-rate=%.0f other-config:priority=%d %s", iqid, a[4], a[5], a[6], burst_string );
			mmp2iq[mmp] = sprintf( "@iq%d", iqid );
			iqid++
		}

	
		if( pt == -128  )							 # for now we will set a queue on all outward interfaces; we need to be smarter and accept @brxxx data instead of -128
		{
			n = split( sw2sw_ports[sw], d, " " );
			for( i = 1; i <= n; i++ )
			{
				pt = d[i];

				if( !switch_has_port[sw,pt] )
				{
					switchports[sw] = switchports[sw] pt " ";
					assigned[sw"-"pt] = 1;								# mark an assignment on this port (seems wrong to test q0max, so we will create a special flag)
				}

				swportq2iq[sw,pt,spq] = mmp2iq[mmp];				# map all ports to the individual queue 
				q0max[sw,pt] -= a[5];								# amount remaining for queue0 on this port
				if( spq > maxq[sw,pt] )								# save the max so we can loop through them later
					maxq[sw,pt] = spq;
				if( spq == 0 )
					saw_queue0[sw,pt] = 1;				# allows data generator (setup_ovs_intermed most likely) to hard set a definition for q0 rather than computing it

				switch_has_port[sw,pt] = 1				# only need -128 port to list once in switcports[]
			}
		}
		else														# just account for the single port
		{
			if( egress_queue )										# only if setting queues on egress interface to VM
			{
				if( ! switch_has_port[sw,pt] )						# only collect port number once if supplied on multiple queues
				{
					switchports[sw] = switchports[sw] pt " "		# collect the list of ports we need to set queues for
					assigned[sw"-"pt] = 1;							# mark an assignment on this port (seems wrong to test q0max, so we will create a special flag)
				}

				swportq2iq[sw,pt,spq] = mmp2iq[mmp];				# map the port to the individual queue 
				q0max[sw,pt] -= a[5];								# amount remaining for queue0 on this port
				if( spq > maxq[sw,pt] )								# save the max so we can loop through them later
					maxq[sw,pt] = spq;
				if( spq == 0 )
					saw_queue0[sw,pt] = 1;				# allows data generator (setup_ovs_intermed most likely) to hard set a definition for q0 rather than computing it

				switch_has_port[sw,pt] = 1
			}
		}
	
	}

	END {
		for( x in iq )						# put out the individual queue definitions first (all but the q0 iqs)
			printf( " %s\n", iq[x] );

		for( s in switchports )					# compute queue 0 individual queues based on other queue usages (only if q0 was not supplied in the data)
		{
			n = split( switchports[s], a, " " );
			for( i = 1; i <= n; i++ )					# set up individual queues for queue 0 needs based on switch/port q0max values  (only if q0 was not supplied in the data
			{
				p = a[i];
				if( !saw_queue0[s,p] )					#if queue 0 was not set in the input data for this switch/port pair
				{
					mv = q0max[s,p];
					mvs = sprintf( "%.0f", mv );		# if mv is really large (e.g. 10G) awk converts to exponential rep for the hash and precision is lost; convert to string to avoid
					if( mv < 1000 )
						mv = 1000						# prevent setting the default queue to 0
					if( q0iq[mvs] == "" )
					{
						q0iq[mvs] = sprintf( "@iq%d", iqid );	# set the individual queue id for this q0 value
						if( static_q0size )
							printf( " -- --id=@iq%d create Queue other-config:min-rate=1000 other-config:max-rate=10000000000 other-config:priority=%d %s\n", iqid, 1000, burst_string );
						else
							printf( " -- --id=@iq%d create Queue other-config:min-rate=%.0f other-config:max-rate=%.0f other-config:priority=%d %s\n", iqid, mv, mv, 1000, burst_string );
						iqid++;
					}

					swportq2iq[s,p,0] = q0iq[mvs];		# map the switch port to a queue 0
				}
			}
		}

		for( s in switchports )				# define the queue combinations (qos entries in ovs terms)
		{
			n = split( switchports[s], a, " " );
			for( i = 1; i <= n; i++ )
			{
				p = a[i]
				printf( " -- --id=@qc%d create QoS %s other-config:max-rate=%.0f queues=", qcid, qtype, max_rate )
				sep = "";
				for( q = 0; q <= maxq[s,p]; q++ )
				{
					if( swportq2iq[s,p,q] != "" )						# add each queue that was defined for the switch/port
					{
						printf( "%s%d=%s", sep, q, swportq2iq[s,p,q] );
						sep = ",";
					}
				}

				swpt2cq[s,p] = sprintf( "@qc%d", qcid );			# map switch/port to the queue combo
				printf( "\n" );
				qcid++;
			}
		}

		for( s in switchports )				# finally, map each switch/port to a queue combo using the sw/port uuid we mapped from the ovs_sp2uuid output
		{
			n = split( switchports[s], a, " " );
			for( i = 1; i <= n; i++ )
			{
				p = a[i];
				if( swpt2cq[s,p] != ""  &&  swpt2uuid[s,p] != "" )
					printf( " -- set Port %s qos=%s \n", swpt2uuid[s,p], swpt2cq[s,p] );
			}
		}

		for( sp in assigned )				# run over all switches and write commands that delete qos from the switch/port if a new one was not assigned
		{
			if( assigned[sp] < 0 )
			{
				split( sp, a, "-" );		# split the name
				if( swpt2uuid[a[1],a[2]] != "" )
					printf( "ovs-vsctl clear Port %s qos\n", swpt2uuid[a[1],a[2]] );
			}
		}
	}
'  | while read buf				# collect all of the ovs command fragments and build into a single ovs command "tail"
do								# CAUTION:  any use of ssh in the loop MUST have -n on the command line
	if (( verbose ))
	then
		logit "$buf" 
	fi

	case $buf in 
		*ERROR!*)	
				logit "CRI: error generating ovs data target-host: ${rhost#* }   [FAIL]  [QLTCOQ001]"
				exit 1
				;;

		ovs-vsctl*)							# direct commands to execute  possibly on a remote host
				if (( forreal ))
				then
					echo "$sudo $buf" >>$cmd_file 				# bundle these to run as one ssh session (faster)
				else
					logit "no-execute: $sudo $buf"
				fi
				;;

		*)									# command segment to add to the tail
				cmd_tail+=" $buf"			# CAUTION: leading spaces chopped by read
				;;
	esac
done 

if [[ -n $cmd_tail ]]   
then
	if (( forreal ))
	then
		echo "$sudo ovs-vsctl $cmd_tail" >>$cmd_file		# add the command to set the queues
	else
		logit "no-execute:  $ssh_host $sudo ovs-vsctl $cmd_tail" 
	fi
fi

rc=0
if (( forreal ))  &&   [[ -s $cmd_file ]]					# empty if not in forreal mode, but take no chances
then
	logit "running ovs command batch"
	set -x
	timeout 15   $ssh_host /bin/ksh -x <$cmd_file		# one ssh session to execute the batch 
	rc=$?
	set +x
	if (( rc > 100 ))			# timeout should exit 124
	then
		logit "command to $rhost timed-out; retrying"
		timeout 15  $ssh_host /bin/ksh -x <$cmd_file		# one ssh session to execute the batch 
		rc=$?
	fi
	if (( rc > 0 ))
	then
		logit "CRI: unable to set queues on bridges, target-host: ${rhost#* }    [FAIL]  [QLTCOQ000]"
	fi
fi

rm -f /tmp/PID$$.*

if (( purge_ok ))					# purge ok is off if -n is set (TODO: for speed it might be nice to bundle these in the earlier ssh batch)
then
	purge_ovs_queues $rhost			# purge any queues that aren't referenced by switches (ignore errors here as leaving the unreferenced queues won't hurt)
fi

if (( delete_data ))
then
	rm -f $data_file
fi

exit

# tegu data layout: switc/port,resname,queue,min,max,priority
# sample tegu data (non-late binding)
#	00:00:00:00:00:00:00:06/3,Rres5502_00000,2,75,75,200
#	00:00:00:00:00:00:00:02/3,priority-out,1,25,25,200
#	00:00:00:00:00:00:00:02/1,priority-in,1,75,75,200
#	00:00:00:00:00:00:00:01/1,priority-in,1,75,75,200
#	00:00:00:00:00:00:00:01/2,priority-out,1,25,25,200
#	00:00:00:00:00:00:00:03/3,res5502_00000,2,25,25,200
#	00:00:00:00:00:00:00:05/1,priority-out,1,25,25,200
#	00:00:00:00:00:00:00:05/3,priority-in,1,75,75,200

# tegu data (late binding)
#qos106/-128,Rres3fbe_00000,2,10000000,10000000,200
#qos102/-128,res3fbe_00000,2,20000000,20000000,200
#qos106/fa:de:ad:7a:3a:72,E1res3fbe_00000,2,20000000,20000000,200
#qos102/fa:de:ad:cc:48:f9,E0res3fbe_00000,2,10000000,10000000,200

