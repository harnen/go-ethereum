features =  {'nodes': {'type': 'benign', 'default': 50,'defaultAttack': 100, 'vals':[50]},
             'topic': {'type': 'benign', 'default': 1, 'defaultAttack': 1,'vals':[1]},
             'regBucketSize': {'type': 'benign', 'default': 10, 'defaultAttack': 10, 'vals':[10]},
             'searchBucketSize': {'type': 'benign', 'default': 3, 'defaultAttack': 3, 'vals':[3]},
             'adLifetimeSeconds': {'type': 'benign', 'default': 60, 'defaultAttack': 60, 'vals':[60]},
             'adCacheSize': {'type': 'benign', 'default': 500, 'defaultAttack': 500, 'vals':[500]},
             'rpcBasePort': {'type': 'benign', 'default': 20200, 'defaultAttack': 20200, 'vals':[20200]},
             'udpBasePort': {'type': 'benign', 'default': 30200, 'defaultAttack': 30200, 'vals':[30200]},
             'returnedNodes': {'type': 'benign', 'default': 30, 'defaultAttack': 1, 'vals':[30]},
}


result_dir = './discv5_test_logs'