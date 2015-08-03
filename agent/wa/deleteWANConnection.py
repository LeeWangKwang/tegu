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
if len(argv) < 5:
  exit('Usage: ' +\
        argv[0] +\
        ' token wan-subnet-uuid router-or-subnet-uuid CIDR [tos]')

token = {'X-Auth-Token': argv[1]}

wan = argv[2]

lockfd = open(lockFile,  'w')
flock(lockfd, LOCK_EX)

router = routerFor(argv[3], token)
if(router == None):
  router = argv[3]

destCIDR = argv[4]

if(len(argv)  > 5):
  tos = argv[5]
else:
  tos = '0'

req = HTTPConnection(NeutronHost, NeutronPort)
wanPort = routerPortFor(router, wan, token)
if(wanPort):
  routerNS = NS_PREFIX + router

  # 1. grab the old route, we'll need the interface name.
  # produces ['10.7.0.0', 'dev', 'gre-90db5528-0d',  'src', '10.0.2.1']
  tunnelIF = subprocess.check_output(['/sbin/ip', 'netns', 'exec', routerNS,
                                      '/sbin/ip', 'route', 'get', destCIDR,
                                      'tos', tos]).splitlines()[0].split()[2]
  # 2. grab the GRE interface IP, we'll need it to remove it from the port
  # produces ['link/gre', '10.254.2.2', 'peer', '10.254.1.1']
  tunnelIP = subprocess.check_output(['/sbin/ip', 'netns', 'exec', routerNS,
                                      '/sbin/ip', 'address', 'show',
                                      tunnelIF]).splitlines()[1].split()[1]
  # ... and the port device name, so we can remove its address
  # produces ['629:', 'qr-dc2c3783-84:', ...]
  portIF = subprocess.check_output(['/sbin/ip', 'netns', 'exec', routerNS,
                                    '/sbin/ip', 'address', 'show', 'to',
                                    tunnelIP]).splitlines()[0].split()[1]
  portIF = portIF.rstrip(':')
  if(len(filter(lambda ip:
                  ip['ip_address'] == tunnelIP,
                wanPort['fixed_ips'])) < 1):
    # this is bogus, obviously: if we're being called to clean up after
    # a failed attempt to create the connection, which managed to create
    # the port and tunnel but not the route, we'll bail for the wrong reason
    # and fail to blow away the tunnel and/or port.  I can't think of a good
    # fix that doesn't risk nuking legitimate ports/tunnels outside our
    # purview.
    exit('Route not found')
  # 3. delete the route
  subprocess.call(['/sbin/ip', 'netns', 'exec', routerNS,
                   '/sbin/ip', 'route', 'delete', destCIDR,
                   'tos', tos, 'dev', tunnelIF])
  # 4. delete the tunnel
  subprocess.call(['/sbin/ip', 'netns', 'exec', routerNS,
                   '/sbin/ip', 'tunnel', 'del', tunnelIF])
  # 5. delete the IP, or the port if it's the last remaining address
  subprocess.call(['/sbin/ip', 'netns', 'exec', routerNS,
                   '/sbin/ip', 'address', 'del', tunnelIP, 'dev', portIF])
  if(len(wanPort['fixed_ips']) > 1):
    req.request('PUT',
                portDetailsPath +'/'+ wanPort['id'] +'.json',
                json.dumps({'port':
                              {'fixed_ips':
                                filter(lambda ip:
                                         ip['ip_address'] != tunnelIP,
                                         wanPort['fixed_ips'])
                              }
                           }),
                token)
  else:
   # ...  remove port from router
   req.request('PUT',
               routersPath +'/'+ router +'/remove_router_interface.json',
               json.dumps({'port_id': wanPort['id']}),
               token)
   # we don't care about the response, but this will throw an exception if
   # things go sideways.
   json.loads(req.getresponse().read())
   # i don't think this is necessary; remove_router_interface seems to
   # delete the port (neutron/db/l3_db.py:403).  that seems like kind of
   # a strange thing for it to do but, well, openstack.
   # req.request('DELETE',
   #             portDetailsPath +'/'+ wanPort['id'] +'.json',
   #             '',
   #             token)
   # json.loads(req.getresponse().read())

flock(lockfd, LOCK_UN)
req.close()
