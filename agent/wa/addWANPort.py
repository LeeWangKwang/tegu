#!/usr/bin/python
# ---------------------------------------------------------------------------
#   Copyright (c) 2013-2015 AT&T Intellectual Property
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at:
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.
# ---------------------------------------------------------------------------


from sys import argv, exit
from httplib import HTTPConnection
from uuid import UUID
from fcntl import flock, LOCK_EX, LOCK_UN
import json
import subprocess

lockFile = '/tmp/WAQoSAgent.lock'

# fixme: these should come from keystone.
NeutronHost='neutron-host.example.com'
NeutronPort=9696
NeutronPath = '/v2.0'
# constant from l3_agent.py
NS_PREFIX='qrouter-'
#
routerListPath = NeutronPath + '/routers.json'
portsPath = NeutronPath + '/ports.json'
subnetDetailsPath = NeutronPath +'/subnets'
routersPath = NeutronPath + '/routers'
portDetailsPath = NeutronPath + '/ports'

#
def routerFor(subnet, token):
  # this isn't really correct -- there could easily be more than one
  # router on the subnet, and we'll only find the first one.  owell.
  subnetUUID = UUID(subnet)
  req = HTTPConnection(NeutronHost, NeutronPort)
  req.request('GET', routerListPath, '', token)
  routerDetailsList = json.loads(req.getresponse().read())
  for routerDetails in routerDetailsList['routers']:
    req.request('GET',
                portsPath + '?device_id=' + routerDetails['id'],
                '',
                token)
    portDetailsList = json.loads(req.getresponse().read())
    for portDetails in portDetailsList['ports']:
     for fixedIP in portDetails['fixed_ips']:
       if UUID(fixedIP['subnet_id']) == subnetUUID:
         req.close()
         return routerDetails['id']
  req.close()
  return None

def routerPortFor(router, subnet, token):
  # this is a little closer to correct: neutron allows only one router
  # interface per subnet.
  subnetUUID = UUID(subnet)
  req = HTTPConnection(NeutronHost, NeutronPort)
  req.request('GET', portsPath + '?device_id=' + router, '', token)
  portDetailsList = json.loads(req.getresponse().read())
  req.close()
  for portDetails in portDetailsList['ports']:
   for fixedIP in portDetails['fixed_ips']:
     if UUID(fixedIP['subnet_id']) == subnetUUID:
       return portDetails
  return None

#
if len(argv) < 4:
  exit('Usage: ' +\
        argv[0] +\
        ' token wan-subnet-uuid router-or-subnet-uuid')

token = {'X-Auth-Token': argv[1]}
wan = argv[2]

# technically, we could probably do this later, but i'd prefer not to
# risk the router changing state while we're in here.
# we really should use taskflow for this, but this should cover us for now
# since all the wategu scripts run on the router's host.
lockfd = open(lockFile,  'w')
flock(lockfd, LOCK_EX)

router = routerFor(argv[3], token)

if(router == None):
  router = argv[3]


req = HTTPConnection(NeutronHost, NeutronPort)

# we can only have one port per router on any given subnet.  find
# out if our router already has a WAN port.  if so, add an address to it.
# otherwise, create a new one and attach it to the router.
# this makes the probably-incorrect assumption that the newly-added address
# is the last one in the fixed_ips list.
wanPort = routerPortFor(router, wan, token)
req.request('GET', subnetDetailsPath +'/'+ wan +'.json', None, token)
wanDetails = json.loads(req.getresponse().read())

if(wanPort):
  req.request('PUT',
              portDetailsPath +'/'+ wanPort['id'] +'.json',
              json.dumps({'port':
                           {'fixed_ips':
                             wanPort['fixed_ips'] + [{'subnet_id': wan}]
                           }
                         }),
              token)
  portDetails = json.loads(req.getresponse().read())
  wanCIDR = wanDetails['subnet']['cidr']
  wanPrefix = wanCIDR.split('/')[-1]
  newAddr = portDetails['port']['fixed_ips'][-1]['ip_address'] +'/'+ wanPrefix
  routerNS = NS_PREFIX + router
  # blech.  adding the port address doesn't actually update the interface.
  # i wish there was a cleaner way to do this, but python doesn't seem to
  # know about setns().
  wanIF = subprocess.check_output(['/sbin/ip', 'netns', 'exec', routerNS,
                                   '/sbin/ip', 'address', 'show',
                                   'to', wanCIDR]).splitlines()[1].split()[-1];
  subprocess.call(['/sbin/ip', 'netns', 'exec', routerNS,
                   '/sbin/ip', 'address', 'add', newAddr, 'dev', wanIF])
else:
  # create the port...
  req.request('POST',
              portsPath,
              json.dumps({'port':
                           {'network_id':
                             wanDetails['subnet']['network_id'],
                            'fixed_ips':[{'subnet_id': wan}],
                            'admin_state_up': True}}),
              token);
  portDetails = json.loads(req.getresponse().read())
  # ... and attach it to the router.
  req.request('PUT',
              routersPath +'/'+ router + '/add_router_interface.json',
              json.dumps({'port_id': portDetails['port']['id']}),
              token);
  # force a throw in case things go sideways:
  json.loads(req.getresponse().read())

flock(lockfd, LOCK_UN)
req.close()

print router +' '+\
      portDetails['port']['id'] +' '+\
      portDetails['port']['fixed_ips'][-1]['ip_address']
