#!/usr/bin/env ruby

require 'excon'
require 'json'

raw = Excon.get('http://localhost:7779/api/state/services').body
data = JSON.parse(raw)

def status_for(status)
  case status
  when 0 then 'Alive'
  when 1 then 'Tombstone'
  when 2 then 'Unhealthy'
  when 3 then 'Unknown'
  end
end

puts
puts 'Events'
puts '-' * 80
data.each do |notification|
  svc = notification['Event']['Service']
  puts "#{'%-30s' % svc['Updated']} #{'%15s' % notification['ClusterName']} #{'%20s' % svc['Hostname']} " +
    "#{'%25s' % svc['Name']} #{svc['Image'].split(/:/).last} " +
    "#{status_for(notification['Event']['PreviousStatus'])} --> " +
    "#{status_for(svc['Status'])}"
end
puts
