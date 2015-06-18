#!/usr/bin/env ksh
# vi: ts=4 sw=4:

#	Mnemonic:	ql_bw_fmods
#	Abstract:	Generates all needed flow-mods on an OVS for a bandwidth reservation
#				between src and dest VMs (-s and -d respectively). Src is considered
#				to be the local VM (the VM that is attached to the OVS, and dest is
#				the remote VM (-l and -r could be used, but src and dest are used in 
#				the other agent scripts so this is being consistent).  In the case where
#				one of the VMs is a router, and an external IP address is supplied and 
#				must be used as the flow-mod match criteria, an indication is needed 
#				as to whether the the external IP address is "associated" with the
#				source or destination VM.  This is indicated by the presence of the -S
#				or -D option on the command line.  
#
#				Bandwidth reservation flow mods are set up this way:
#					inbound 
#						p450 Match:	
#								meta == 0 &&
#								reservation VM0 &&
#								reservation VM1 [&& external IP]
#							 Action:
#								[strip dscp value]
#								set metadata 0x01 (resub 90)
#								resub 0 for openstack fmod application
#
#					outbound
#						p400 Match:
#								meta == 0 &&
#								reservation VM0 &&
#								reservation VM1 [&& external IP]
#							 Action:
#								mark with meta value (resub 90)
#								set dscp value
#								resub 0 to apply openstack fmods
#
#				We no longer need to set VLAN on outbound nor do we need to strip VLAN on inbound, so
#				vlan options are currently ignored (supported to be compatable with old/unchanged
#				agents).  Same with queues. We aren't queuing at the moment so the queue options are
#				ignored. In future, there will (should) be a concept of flow-limits (meters maybe)
#				which will be passed in as queue numbers, so the -q option needs to be kept and should
#				be expected and used when the underlying network compoents can support it.
#							
#	Date:		20 March 2015
# 	Author: 	E. Scott Daniels
#
#	Mods:		22 Mar 2015 - Added keep on exit option.
#				27 Mar 2015 - Added ipv6 support.
#				20 Apr 2015 - Accept external IP direction
#				11 May 2015 - Inbound flow-mods must match all types, changed to allow for this.
#				14 May 2015 - To eliminate the use of br-rl and thus the last HTB queue. (flow-mods are
#								now very simple, one in each direction)
#				28 May 2015 - Added match vlan support (-V)
#				18 Jun 2015 - Better handling of -q allowing HTB shutoff to be affected completely by 
#								agent scripts (Tegu still thinks it's being set!)
# ---------------------------------------------------------------------------------------------------------

function logit
{
	echo "$(date "+%s %Y/%m/%d %H:%M:%S") $argv0: $@" >&2
}

function usage
{
	echo "$argv0 v1.1/15125"
	echo "usage: $argv0 [-6] [-d dst-mac] [-E external-ip] [-h host] [-k] [-n] [-o] [-p|P proto:port] [-s src-mac] [-T dscp] [-t hard-timeout] [-v]"
	echo "usage: $argv0 [-X] # delete all"
	echo ""
	echo "  -6 forces IPv6 address matching to be set"
}


# ----------------------------------------------------------------------------------------------------------

cookie="0xb0ff"			# static for now, but might want to make them user controlled, so set them up here
bridge="br-int"
mt_base=90				# meta table base 90 sets 0x01, 91 sets 0x02, 94 sets 0x04...

lmac=""					# local mac 	src outbound, dest inbound
rmac=""					# remote mac	src inbound, dest outbound
queue=""
idscp=""
odscp=""
host=""
forreal=""
pri_base=0				# priority is bumpped up a bit for protocol specific f-mods
vp_base=0				# priority added if vlan match supplied (outbound)
one_switch=0			# may need to handle things differently if one switch is involved
queue=""
koe=0					# keep dscp value as packet 'exits' our environment. Set if global_* traffic type given to tegu
to_value="61"			# value used to check (without option flag)
timout="-t $to_value"	# timeout parm given on command
operation="add"			# -X allows short time durations for deletes
ip_type="-4"			# default to forcing an IP type match for outbound fmods; inbound fmods do NOT use this
ex_local=1				# the external IP is "associated" with the local when 1 (-S) and with the remote when 0 (-D)

while [[ $1 == -* ]]
do
	case $1 in
		-6)		ip_type="-6";;							# force ip6 option to be given to send_ovs_fmod (outbound only).
		-b)		mt_base="$2"; shift;;
		-d)		rmac="$2"; shift;;
		-D)		ex_local=0;;								# external IP is "associated" with the rmac (-d) address
		-E)		exip="$2"; shift;;
		-h)		host="-h $2"; shift;;
		-k)		koe=1;;
		-n)		forreal="-n";;
		-o)		one_switch=1;;
		-p)		pri_base=5; proto="-p $2"; shift;;		# source proto:port priority must increase to match over more generic f-mods
		-P)		pri_base=5; proto="-P $2"; shift;;		# dest proto:port priority must increase to match over more generic f-mods
		-q)		queue="-q $2"; shift;;					# soon to change to meter
		-s)		lmac="$2"; shift;;
		-S)		ex_local=1;;								# external IP is "associaetd" with the lmac (-s) address.
		-t)		to_value=$2; timeout="-t $2"; shift;;
		-T)		odscp="-T $2"; shift;;
		-v)		set_vlan=0;;							# ignored -- maintained for backwards compat
		-V)		vp_base=5; match_vlan="-v $2"; shift;;	# vlan id given on resrvation for match (applies only to outbound)
		-X)		operation="del";;

		-\?)	usage
				exit 0
				;;

		*)	echo "unrecognised option: $1"
			usage
			exit 1
			;;
	esac

	shift
done

# CAUTION:  this is confusing, so be careful (see notes in flower box at top)
if [[ -n $exip ]]						# need to set up matching for external 
then
	if (( ex_local ))					# the lmac is associated with the external IP address
	then
		oexip="-S $exip"				# for outbound, the external ip is the src
		iexip="-D $exip"				# for inbound the external ip is the dest
	else
		oexip="-D $exip"				# rmac is associated with external IP, thus outbound external IP is dest
		iexip="-S $exip"				# and inbound the external is source.
	fi
else
	oexip=""
	iexip=""
fi

if [[ -z $lmac || -z $rmac ]]
then
	logit "must have soruce and dest mac addresses in order to generate flow-mods   [FAIL]"
	exit 1
fi

if (( koe ))
then
	idscp=""			# don't reset the dscp value on inbbound (exiting) traffic
else
	idscp="-T 0"
fi

if [[ -n $queue ]]
then
	echo "ignoring -q setting: htb queues not allowed   [OK]"
	queue=""
fi

# CAUTION: action options to send_ovs_fmods are probably order dependent, so be careful.
if (( ! one_switch ))
then
	# inbound -- only if both are not on the same switch
	send_ovs_fmod $forreal $host $timeout -p $(( 450 + pri_base )) --match $ip_type -m 0x0/0x7 $iexip -d $lmac -s $rmac $proto --action $queue $idscp -M 0x01 -R ,0 -N $operation $cookie $bridge
	rc=$?
else
	if (( ! koe ))		# one switch and keep is off, no need to set dscp 
	then
		odscp=""
	fi
fi

#outbound
send_ovs_fmod $forreal $host $timeout -p $(( 400 + vp_base + pri_base )) --match  $match_vlan $ip_type -m 0x0/0x7 $oexip -s $lmac -d $rmac $proto --action $queue $odscp -M 0x01  -R ,0 -N $operation $cookie $bridge
rc=$(( rc + $? ))

rm -f /tmp/PID$$.*
if (( rc ))
then
	exit 1
fi

exit 0
