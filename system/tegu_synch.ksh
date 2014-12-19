#!/usr/bin/env ksh
#
#	Mnemonic:	tegu_synch
#	Abstract:	Simple script to take a snapshot of the checkpoint environment and push it off to 
#				the stand-by hosts.  Stand-by hosts are expected to be listed one per line in 
#				If the first parameter on the command line is "recover" then this script will
#				attempt to restore the most receent synch file into the chkpt directory. 
#
#				CAUTION:  A _huge_ assumption is made here -- the TEGU_ROOT directory on each 
#					host is the same!
#
#   Exit:		an exit code of 1 is an error while an exit code of 2 is a warning and the calling
#				script might be able to ignore it depending on what action was attempted. An exit
#				code of zero is good.
#
#	Date:		25 July 2014
#	Author:		E. Scott Daniels
#
#	Mod:
# --------------------------------------------------------------------------------------------------

trap "rm -f /tmp/PID$$.*" 1 2 3 15 EXIT

function verify_id
{
	whoiam=$( id -n -u )
	if [[ $whoiam != $tegu_user ]]
	then
		echo "Only the tegu user ($tegu_user) can affect the state; ($(whoami) is not acceptable)     [FAIL]"
		echo "'sudo su $tegu_user' and rerun this script"
		echo ""
		exit 1
	fi
}

# check for standby mode and bail  if this is a standby node
function ensure_active
{

	if [[ -f $standby_file ]]
	then
		echo "WRN: this host is a tegu standby host and does not synch its files"
		exit 0
	fi
}

# capture a config file
function cap_config
{
	if [[ -f $etcd/$1 ]]
	then
		if ! cp $etcd/$1 chkpt/
		then
			echo "WRN: unable to capture a copy of the config file ($etcd/$1) with the check point files" >&2
		fi
	else
		echo "WRN: $etcd/$1  does not exist, config was captured with the checkpoint files" >&2
	fi
}

# restore a configuration file
function restore_config
{
	if [[ -f $1 ]]				# if a config file was captured with the checkpoint files
	then
		if  cp $1 $etcd/
		then
			echo "config file ($1) restored and copied into $etcd    [OK]" >&2
		else
			echo "WRN: unable to copy config file ($1) into $etcd" >&2
		fi
	fi
}

# --------------------------------------------------------------------------------------------------

export TEGU_ROOT=${TEGU_ROOT:-/var}
logd=${TEGU_LOGD:-/var/log/tegu}
libd=${TEGU_LIBD:-/var/lib/tegu}
etcd=${TEGU_ETCD:-/etc/tegu}
chkptd=$TEGU_ROOT/chkpt
tegu_user=${TEGU_USER:-tegu}

ssh_opts="-o StrictHostKeyChecking=no -o PreferredAuthentications=publickey"

standby_file=$etcd/standby
restore=0

case $1 in 
	restore)	#restore the latest sync into the chkpt directory
		restore=1
		;;

	*)	ensure_active;;
esac

if [[ ! -d $etcd ]]
then
	echo "WRN: tegu seems not to be installed on this host: $etcd doesn't exist" >&2
	exit 1
fi

verify_id			# ensure we're running with tegu user id

if ! cd $libd
then
	echo "CRI: unable to switch to tegu lib directory: $libd   [FAIL]" >&2
	exit 1
fi

if [[ ! -d chkpt ]]
then
	if (( restore ))
	then
		if ! mkdir chkpt
		then
			echo "CRI: unable to create the checkpoint directory $PWD/chkpt" >&2
			exit 1
		fi
	else
		echo "WRN: no checkpoint directory exists on this host, nothing done" >&2
		exit 2
	fi
fi


if (( ! restore ))				# take a snap shot of our current set of chkpt files and the current config from $etcd
then
	if [[ ! -f $etcd/standby_list ]]
	then
		echo "WRN: no stand-by list ($etcd/standby_list), nothing done" >&2
		exit 2
	fi
	
	cap_config tegu.cfg					# we need to snarf several of the current config files too
	ls $etcd/*.json | while read jfile
	do
		cap_config ${jfile##*/}
	done

	m=$( date +%M )						# current minutes
	n=$(( (m/5) * 5 ))					# round current minutes to previous 5 min boundary
	host=$( hostname )
	tfile=/tmp/PID$$.chkpt.tgz			# local tar file
	rfile=$libd/chkpt_synch.$host.$n.tgz	# remote archive (we should save just 12 so no need for cleanup)
	tar -cf - chkpt |gzip >$tfile
	
	while read host
	do
		if ! scp $ssh_opts -o PasswordAuthentication=no $tfile $tegu_user@$host:$rfile
		then
			echo "CRI: unable to copy the synch file to remote host $host" >&2
		else
			echo "successful copy of sync file to $host   [OK]"
		fi
	done <$etcd/standby_list
else
	ls -t $libd/chkpt_synch.*.*.tgz | head -1 |read synch_file
	if [[ -z $synch_file ]]
	then
		echo "WRN: cannot find a synch file, no restore of synchronised data" >&2
		exit 2
	fi

	bfile=$libd/synch_backup.tgz		# we'll take a snapshot of what was there just to prevent some accidents
	tar -cf - chkpt | gzip >$bfile

	gzip -dc $synch_file | tar -xf - 		# unload the synch file into the directory
	echo "synch file ($synch_file) was restored into $PWD/chkpt    [OK]"

	restore_config chkpt/tegu.cfg					# restore the config files 
	ls chkpt/*.json | while read jfile
	do
		restore_config $jfile
	done
fi

exit 0
