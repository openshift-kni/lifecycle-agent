package imagemgmt

import (
	"reflect"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/samber/lo"
	"go.uber.org/mock/gomock"
)

// Fake JSON output of crictl and podman commands for test purposes, where:
// - Every third image, starting at 0, is pinned
// - Every second image, starting at 0, is in-use, alternating between crictl and podman
// - Each image is one second "newer" than the previous

const test_CrictlImagesOutput = `
{
  "images": [
    {
      "id": "fac47afb3425a04d078de14cba99f808ca23929baa2bd4880537f7315153b285",
      "repoTags": [
        "localhost/testimage:0"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:2e271416d1eb7670ddf263617d09e5b84e72a09d5157d74c668d906ea3bbdd9c"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "119b2b0abddc725b93a5c80444c10dc826bde57faae9c7bc41a9f8cbb0762520",
      "repoTags": [
        "localhost/testimage:1"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:dbd9a153d403981c6bf8626f3631ac05850808c97ff60f3edfe8bb7158e4b4e4"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "205863aa5d8207661746041f89780fc8ca3a4d8e559f4ecc9aa3a74172cb297d",
      "repoTags": [
        "localhost/testimage:2"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:7b1d4378aa4c977da0d2cb22f15636fb4488abaf304ae6eb044a17dc51ebbeaf"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "07e50e11b9ce6b119fe73af3f65197d40528483b332c4cf523ccb58bd2593582",
      "repoTags": [
        "localhost/testimage:3"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:f425db773e8c4548db956d1b9b6a04cce71016bb185af67a60a2fcc5288e1d50"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "94efb4a28414750992f89709467fe85371decd8f2e25b5e0fd941a3ceeedfff8",
      "repoTags": [
        "localhost/testimage:4"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:26ec4197ef422107291cddcec6a024e86995a4f0833706e87e212deaadfce51c"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "ae7fe988bde07ce7ae026ce17e17dac5cb973d9a5655e0cf8bc61a5feebf740f",
      "repoTags": [],
      "repoDigests": [],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "374612890bd4b9a083298b2384e2d174c99888cbaf158de8733de3dc298f42f7",
      "repoTags": [
        "localhost/testimage:6"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:07def517d449732e4975668b801ca4c2866891a2602d84029d622f1533f7016a"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "6b6e9b1fcd2019e159605c7089e9079a21ace010e0e5ff6da5b74fd613c0b290",
      "repoTags": [
        "localhost/testimage:7"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:0589f1275b641237c935cdf6c4826d7a753007e8a9502f4f15bf1be82b8ea7bb"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "93ceb217a3418237d726c89dec1ec90b7e3123fb10db1c066d3e514b029cbd88",
      "repoTags": [
        "localhost/testimage:8"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:1f715b5480c64ed4d0a7512861c2e6dcb6812842df2e1e5c5866e1678150e7a4"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "8cf728726c737338e2d582e26d17114012b84622be1367cf8bbfdbbf9110d0c9",
      "repoTags": [
        "localhost/testimage:9"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:86e62f7ebe5cff353f791bb22a94997b8bd35952014cfe3a16f62580d447e83d"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "228caa0c29b9304ad14a5ecae8426c72dd91b35ded4a6cf6674d23b475452697",
      "repoTags": [
        "localhost/testimage:10"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:b5549517a1b3bc86ef83c9de9dcb1a47d1bbe3edfe4d8302bfa5b0f73f39c3d9"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "9f788bab3c6cdb77c9c7e89c4f5571f7940ae2aff2ad3bd555c6939668b59ae9",
      "repoTags": [],
      "repoDigests": [],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "f71e66b855a82e2a2fff1b2fa14fa75dec80aff06969b0b3458ba147aa17a1dd",
      "repoTags": [
        "localhost/testimage:12"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:41465d8e61d4c99e4c8fa3bc0da9ba973fa7942c776545aca2edcb69f86d7430"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "2ce8f4694204fb2197d5648b3aed1b40786db6fe478dd59defe580a18e4a95d8",
      "repoTags": [
        "localhost/testimage:13"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:57c9954bbaae589c9bbb5b4f2640087963aad080f0979ea7a15b9f3f7032967b"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "370c581248a206d5fd2ec6c8da2588a0d6f943512748bec6a18dab8a01c2c455",
      "repoTags": [
        "localhost/testimage:14"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:68831587783c8c5501f89d025b038b7da46995e62cba7a6c5d4f7f18b54274ea"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "f248cca1866963e6375321492d764908807c0ca860ef1d78109583d63d38b276",
      "repoTags": [
        "localhost/testimage:15"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:2ad41fbd07ff5a2baf74c7c6c064cf0b8ceb95202c935903a1c320e7e507ee76"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "e9b78616bc4663607bea58a560d44ca524f98b6e71dc1c68fbd394a125638c8d",
      "repoTags": [
        "localhost/testimage:16"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:bc2f2bb8bed03afe8b40c47a5c2b0884c15dc4862e598e795b91b7d9a016d4ca"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "b27b9a144cffa63b0ff4b8e466da6ea32c11e46a38995cfb62a01146d9b7cd5b",
      "repoTags": [],
      "repoDigests": [],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    },
    {
      "id": "50e3dd8976524b2be798c5a46f72ae1bef84d6a41514bde72eed740676e79412",
      "repoTags": [
        "localhost/testimage:18"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:cb41342374aa9f4ef9724b606c34876f0acac8da0db581ec975dfb152058d330"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": true
    },
    {
      "id": "2a1bf62013de91736e9dcd2297fa78350544dfaecf4653c53af704e85ecfaec5",
      "repoTags": [
        "localhost/testimage:19"
      ],
      "repoDigests": [
        "localhost/testimage@sha256:1131019c31d160f96b5219843321653b54c68f388953a262c491da640142101b"
      ],
      "size": "1073744835",
      "uid": null,
      "username": "",
      "spec": null,
      "pinned": false
    }
  ]
}
`

const test_PodmanImagesOutput = `
[
    {
        "Id": "fac47afb3425a04d078de14cba99f808ca23929baa2bd4880537f7315153b285",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:2e271416d1eb7670ddf263617d09e5b84e72a09d5157d74c668d906ea3bbdd9c"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:0"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943200,
        "CreatedAt": "2024-07-02:18:00:00Z"
    },
    {
        "Id": "119b2b0abddc725b93a5c80444c10dc826bde57faae9c7bc41a9f8cbb0762520",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:dbd9a153d403981c6bf8626f3631ac05850808c97ff60f3edfe8bb7158e4b4e4"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:1"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943201,
        "CreatedAt": "2024-07-02:18:00:01Z"
    },
    {
        "Id": "205863aa5d8207661746041f89780fc8ca3a4d8e559f4ecc9aa3a74172cb297d",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:7b1d4378aa4c977da0d2cb22f15636fb4488abaf304ae6eb044a17dc51ebbeaf"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:2"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943202,
        "CreatedAt": "2024-07-02:18:00:02Z"
    },
    {
        "Id": "07e50e11b9ce6b119fe73af3f65197d40528483b332c4cf523ccb58bd2593582",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:f425db773e8c4548db956d1b9b6a04cce71016bb185af67a60a2fcc5288e1d50"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:3"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943203,
        "CreatedAt": "2024-07-02:18:00:03Z"
    },
    {
        "Id": "94efb4a28414750992f89709467fe85371decd8f2e25b5e0fd941a3ceeedfff8",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:26ec4197ef422107291cddcec6a024e86995a4f0833706e87e212deaadfce51c"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:4"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943204,
        "CreatedAt": "2024-07-02:18:00:04Z"
    },
    {
        "Id": "ae7fe988bde07ce7ae026ce17e17dac5cb973d9a5655e0cf8bc61a5feebf740f",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [],
        "Dangling": true,
        "Digest": "sha256:notused",
		"History": [
			"localhost/testimage:5"
		],
		"Created": 1719943205,
        "CreatedAt": "2024-07-02:18:00:05Z"
    },
    {
        "Id": "374612890bd4b9a083298b2384e2d174c99888cbaf158de8733de3dc298f42f7",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:07def517d449732e4975668b801ca4c2866891a2602d84029d622f1533f7016a"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:6"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943206,
        "CreatedAt": "2024-07-02:18:00:06Z"
    },
    {
        "Id": "6b6e9b1fcd2019e159605c7089e9079a21ace010e0e5ff6da5b74fd613c0b290",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:0589f1275b641237c935cdf6c4826d7a753007e8a9502f4f15bf1be82b8ea7bb"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:7"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943207,
        "CreatedAt": "2024-07-02:18:00:07Z"
    },
    {
        "Id": "93ceb217a3418237d726c89dec1ec90b7e3123fb10db1c066d3e514b029cbd88",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:1f715b5480c64ed4d0a7512861c2e6dcb6812842df2e1e5c5866e1678150e7a4"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:8"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943208,
        "CreatedAt": "2024-07-02:18:00:08Z"
    },
    {
        "Id": "8cf728726c737338e2d582e26d17114012b84622be1367cf8bbfdbbf9110d0c9",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:86e62f7ebe5cff353f791bb22a94997b8bd35952014cfe3a16f62580d447e83d"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:9"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943209,
        "CreatedAt": "2024-07-02:18:00:09Z"
    },
    {
        "Id": "228caa0c29b9304ad14a5ecae8426c72dd91b35ded4a6cf6674d23b475452697",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:b5549517a1b3bc86ef83c9de9dcb1a47d1bbe3edfe4d8302bfa5b0f73f39c3d9"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:10"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943210,
        "CreatedAt": "2024-07-02:18:00:10Z"
    },
    {
        "Id": "9f788bab3c6cdb77c9c7e89c4f5571f7940ae2aff2ad3bd555c6939668b59ae9",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [],
		"Dangling": true,
		"Digest": "sha256:notused",
		"History": [
			"localhost/testimage:11"
		],
        "Created": 1719943211,
        "CreatedAt": "2024-07-02:18:00:11Z"
    },
    {
        "Id": "f71e66b855a82e2a2fff1b2fa14fa75dec80aff06969b0b3458ba147aa17a1dd",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:41465d8e61d4c99e4c8fa3bc0da9ba973fa7942c776545aca2edcb69f86d7430"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:12"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943212,
        "CreatedAt": "2024-07-02:18:00:12Z"
    },
    {
        "Id": "2ce8f4694204fb2197d5648b3aed1b40786db6fe478dd59defe580a18e4a95d8",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:57c9954bbaae589c9bbb5b4f2640087963aad080f0979ea7a15b9f3f7032967b"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:13"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943213,
        "CreatedAt": "2024-07-02:18:00:13Z"
    },
    {
        "Id": "370c581248a206d5fd2ec6c8da2588a0d6f943512748bec6a18dab8a01c2c455",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:68831587783c8c5501f89d025b038b7da46995e62cba7a6c5d4f7f18b54274ea"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:14"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943214,
        "CreatedAt": "2024-07-02:18:00:14Z"
    },
    {
        "Id": "f248cca1866963e6375321492d764908807c0ca860ef1d78109583d63d38b276",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:2ad41fbd07ff5a2baf74c7c6c064cf0b8ceb95202c935903a1c320e7e507ee76"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:15"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943215,
        "CreatedAt": "2024-07-02:18:00:15Z"
    },
    {
        "Id": "e9b78616bc4663607bea58a560d44ca524f98b6e71dc1c68fbd394a125638c8d",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:bc2f2bb8bed03afe8b40c47a5c2b0884c15dc4862e598e795b91b7d9a016d4ca"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:16"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943216,
        "CreatedAt": "2024-07-02:18:00:16Z"
    },
    {
        "Id": "b27b9a144cffa63b0ff4b8e466da6ea32c11e46a38995cfb62a01146d9b7cd5b",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [],
		"Dangling": true,
		"Digest": "sha256:notused",
		"History": [
			"localhost/testimage:17"
		],
        "Created": 1719943217,
        "CreatedAt": "2024-07-02:18:00:17Z"
    },
    {
        "Id": "50e3dd8976524b2be798c5a46f72ae1bef84d6a41514bde72eed740676e79412",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:cb41342374aa9f4ef9724b606c34876f0acac8da0db581ec975dfb152058d330"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:18"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943218,
        "CreatedAt": "2024-07-02:18:00:18Z"
    },
    {
        "Id": "2a1bf62013de91736e9dcd2297fa78350544dfaecf4653c53af704e85ecfaec5",
        "ParentId": "",
        "RepoTags": null,
        "RepoDigests": [
            "localhost/testimage@sha256:1131019c31d160f96b5219843321653b54c68f388953a262c491da640142101b"
        ],
        "Size": 1073744835,
        "SharedSize": 0,
        "VirtualSize": 1073744835,
        "Labels": {
            "notused": "notused"
        },
        "Containers": 0,
        "Names": [
            "localhost/testimage:19"
        ],
        "Digest": "sha256:notused",
        "Created": 1719943219,
        "CreatedAt": "2024-07-02:18:00:19Z"
    }
]
`

const test_CrictlPsOutput = `
{
  "containers": [
    {
      "id": "unused",
      "podSandboxId": "unused",
      "metadata": {
        "name": "anyname",
        "attempt": 0
      },
      "image": {
        "image": "fac47afb3425a04d078de14cba99f808ca23929baa2bd4880537f7315153b285",
        "annotations": {
        },
        "userSpecifiedImage": ""
      },
      "imageRef": "unused",
      "state": "unused",
      "createdAt": "1719931500784052855",
      "labels": {
        "unused": "unused"
      },
      "annotations": {
        "unused": "unused"
      }
    },
    {
      "id": "unused",
      "podSandboxId": "unused",
      "metadata": {
        "name": "anyname",
        "attempt": 0
      },
      "image": {
        "image": "94efb4a28414750992f89709467fe85371decd8f2e25b5e0fd941a3ceeedfff8",
        "annotations": {
        },
        "userSpecifiedImage": ""
      },
      "imageRef": "unused",
      "state": "unused",
      "createdAt": "1719931500784052855",
      "labels": {
        "unused": "unused"
      },
      "annotations": {
        "unused": "unused"
      }
    },
    {
      "id": "unused",
      "podSandboxId": "unused",
      "metadata": {
        "name": "anyname",
        "attempt": 0
      },
      "image": {
        "image": "93ceb217a3418237d726c89dec1ec90b7e3123fb10db1c066d3e514b029cbd88",
        "annotations": {
        },
        "userSpecifiedImage": ""
      },
      "imageRef": "unused",
      "state": "unused",
      "createdAt": "1719931500784052855",
      "labels": {
        "unused": "unused"
      },
      "annotations": {
        "unused": "unused"
      }
    },
    {
      "id": "unused",
      "podSandboxId": "unused",
      "metadata": {
        "name": "anyname",
        "attempt": 0
      },
      "image": {
        "image": "f71e66b855a82e2a2fff1b2fa14fa75dec80aff06969b0b3458ba147aa17a1dd",
        "annotations": {
        },
        "userSpecifiedImage": ""
      },
      "imageRef": "unused",
      "state": "unused",
      "createdAt": "1719931500784052855",
      "labels": {
        "unused": "unused"
      },
      "annotations": {
        "unused": "unused"
      }
    },
    {
      "id": "unused",
      "podSandboxId": "unused",
      "metadata": {
        "name": "anyname",
        "attempt": 0
      },
      "image": {
        "image": "e9b78616bc4663607bea58a560d44ca524f98b6e71dc1c68fbd394a125638c8d",
        "annotations": {
        },
        "userSpecifiedImage": ""
      },
      "imageRef": "unused",
      "state": "unused",
      "createdAt": "1719931500784052855",
      "labels": {
        "unused": "unused"
      },
      "annotations": {
        "unused": "unused"
      }
    }
  ]
}
`

const test_PodmanPsOutput = `
[
  {
    "AutoRemove": true,
    "Command": [
      "anycommand"
    ],
    "CreatedAt": "3 days ago",
    "Exited": false,
    "Id": "unused",
    "Image": "unused",
    "ImageID": "205863aa5d8207661746041f89780fc8ca3a4d8e559f4ecc9aa3a74172cb297d",
    "IsInfra": false,
    "Labels": {
      "unused": "unused"
    },
    "Mounts": [
      "/rootfs"
    ],
    "Names": [
      "anyname"
    ],
    "Namespaces": {
    },
    "Networks": [],
    "Pid": 0,
    "Pod": "",
    "PodName": "",
    "Ports": null,
    "Size": null,
    "StartedAt": 1719588750,
    "State": "created",
    "Status": "Created",
    "Created": 1719588750
  },
  {
    "AutoRemove": true,
    "Command": [
      "anycommand"
    ],
    "CreatedAt": "3 days ago",
    "Exited": false,
    "Id": "unused",
    "Image": "unused",
    "ImageID": "374612890bd4b9a083298b2384e2d174c99888cbaf158de8733de3dc298f42f7",
    "IsInfra": false,
    "Labels": {
      "unused": "unused"
    },
    "Mounts": [
      "/rootfs"
    ],
    "Names": [
      "anyname"
    ],
    "Namespaces": {
    },
    "Networks": [],
    "Pid": 0,
    "Pod": "",
    "PodName": "",
    "Ports": null,
    "Size": null,
    "StartedAt": 1719588750,
    "State": "created",
    "Status": "Created",
    "Created": 1719588750
  },
  {
    "AutoRemove": true,
    "Command": [
      "anycommand"
    ],
    "CreatedAt": "3 days ago",
    "Exited": false,
    "Id": "unused",
    "Image": "unused",
    "ImageID": "228caa0c29b9304ad14a5ecae8426c72dd91b35ded4a6cf6674d23b475452697",
    "IsInfra": false,
    "Labels": {
      "unused": "unused"
    },
    "Mounts": [
      "/rootfs"
    ],
    "Names": [
      "anyname"
    ],
    "Namespaces": {
    },
    "Networks": [],
    "Pid": 0,
    "Pod": "",
    "PodName": "",
    "Ports": null,
    "Size": null,
    "StartedAt": 1719588750,
    "State": "created",
    "Status": "Created",
    "Created": 1719588750
  },
  {
    "AutoRemove": true,
    "Command": [
      "anycommand"
    ],
    "CreatedAt": "3 days ago",
    "Exited": false,
    "Id": "unused",
    "Image": "unused",
    "ImageID": "370c581248a206d5fd2ec6c8da2588a0d6f943512748bec6a18dab8a01c2c455",
    "IsInfra": false,
    "Labels": {
      "unused": "unused"
    },
    "Mounts": [
      "/rootfs"
    ],
    "Names": [
      "anyname"
    ],
    "Namespaces": {
    },
    "Networks": [],
    "Pid": 0,
    "Pod": "",
    "PodName": "",
    "Ports": null,
    "Size": null,
    "StartedAt": 1719588750,
    "State": "created",
    "Status": "Created",
    "Created": 1719588750
  },
  {
    "AutoRemove": true,
    "Command": [
      "anycommand"
    ],
    "CreatedAt": "3 days ago",
    "Exited": false,
    "Id": "unused",
    "Image": "unused",
    "ImageID": "50e3dd8976524b2be798c5a46f72ae1bef84d6a41514bde72eed740676e79412",
    "IsInfra": false,
    "Labels": {
      "unused": "unused"
    },
    "Mounts": [
      "/rootfs"
    ],
    "Names": [
      "anyname"
    ],
    "Namespaces": {
    },
    "Networks": [],
    "Pid": 0,
    "Pod": "",
    "PodName": "",
    "Ports": null,
    "Size": null,
    "StartedAt": 1719588750,
    "State": "created",
    "Status": "Created",
    "Created": 1719588750
  }
]
`

var test_InUseImages = []string{
	"fac47afb3425a04d078de14cba99f808ca23929baa2bd4880537f7315153b285",
	"205863aa5d8207661746041f89780fc8ca3a4d8e559f4ecc9aa3a74172cb297d",
	"94efb4a28414750992f89709467fe85371decd8f2e25b5e0fd941a3ceeedfff8",
	"374612890bd4b9a083298b2384e2d174c99888cbaf158de8733de3dc298f42f7",
	"93ceb217a3418237d726c89dec1ec90b7e3123fb10db1c066d3e514b029cbd88",
	"228caa0c29b9304ad14a5ecae8426c72dd91b35ded4a6cf6674d23b475452697",
	"f71e66b855a82e2a2fff1b2fa14fa75dec80aff06969b0b3458ba147aa17a1dd",
	"370c581248a206d5fd2ec6c8da2588a0d6f943512748bec6a18dab8a01c2c455",
	"e9b78616bc4663607bea58a560d44ca524f98b6e71dc1c68fbd394a125638c8d",
	"50e3dd8976524b2be798c5a46f72ae1bef84d6a41514bde72eed740676e79412",
}

var test_PinnedImages = []string{
	"fac47afb3425a04d078de14cba99f808ca23929baa2bd4880537f7315153b285",
	"07e50e11b9ce6b119fe73af3f65197d40528483b332c4cf523ccb58bd2593582",
	"374612890bd4b9a083298b2384e2d174c99888cbaf158de8733de3dc298f42f7",
	"8cf728726c737338e2d582e26d17114012b84622be1367cf8bbfdbbf9110d0c9",
	"f71e66b855a82e2a2fff1b2fa14fa75dec80aff06969b0b3458ba147aa17a1dd",
	"f248cca1866963e6375321492d764908807c0ca860ef1d78109583d63d38b276",
	"50e3dd8976524b2be798c5a46f72ae1bef84d6a41514bde72eed740676e79412",
}

var test_RemovalCandidates = []imageMgmtImageInfo{
	{
		Id:          "ae7fe988bde07ce7ae026ce17e17dac5cb973d9a5655e0cf8bc61a5feebf740f",
		RepoTags:    nil,
		RepoDigests: []string{},
		Names:       []string{},
		Created:     1719943205,
		Dangling:    true,
	},
	{
		Id:          "9f788bab3c6cdb77c9c7e89c4f5571f7940ae2aff2ad3bd555c6939668b59ae9",
		RepoTags:    nil,
		RepoDigests: []string{},
		Names:       []string{},
		Created:     1719943211,
		Dangling:    true,
	},
	{
		Id:          "b27b9a144cffa63b0ff4b8e466da6ea32c11e46a38995cfb62a01146d9b7cd5b",
		RepoTags:    nil,
		RepoDigests: []string{},
		Names:       []string{},
		Created:     1719943217,
		Dangling:    true,
	},
	{
		Id:       "119b2b0abddc725b93a5c80444c10dc826bde57faae9c7bc41a9f8cbb0762520",
		RepoTags: nil,
		RepoDigests: []string{
			"localhost/testimage@sha256:dbd9a153d403981c6bf8626f3631ac05850808c97ff60f3edfe8bb7158e4b4e4",
		},
		Names: []string{
			"localhost/testimage:1",
		},
		Created: 1719943201,
	},
	{
		Id:       "6b6e9b1fcd2019e159605c7089e9079a21ace010e0e5ff6da5b74fd613c0b290",
		RepoTags: nil,
		RepoDigests: []string{
			"localhost/testimage@sha256:0589f1275b641237c935cdf6c4826d7a753007e8a9502f4f15bf1be82b8ea7bb",
		},
		Names: []string{
			"localhost/testimage:7",
		},
		Created: 1719943207,
	},
	{
		Id:       "2ce8f4694204fb2197d5648b3aed1b40786db6fe478dd59defe580a18e4a95d8",
		RepoTags: nil,
		RepoDigests: []string{
			"localhost/testimage@sha256:57c9954bbaae589c9bbb5b4f2640087963aad080f0979ea7a15b9f3f7032967b",
		},
		Names: []string{
			"localhost/testimage:13",
		},
		Created: 1719943213,
	},
	{
		Id:       "2a1bf62013de91736e9dcd2297fa78350544dfaecf4653c53af704e85ecfaec5",
		RepoTags: nil,
		RepoDigests: []string{
			"localhost/testimage@sha256:1131019c31d160f96b5219843321653b54c68f388953a262c491da640142101b",
		},
		Names: []string{
			"localhost/testimage:19",
		},
		Created: 1719943219,
	},
}

// Test code

func Test_CheckDiskUsageAgainstThreshold(t *testing.T) {
	stat25Pct := syscall.Statfs_t{Blocks: 100, Bavail: 75}
	stat50Pct := syscall.Statfs_t{Blocks: 100, Bavail: 50}
	stat75Pct := syscall.Statfs_t{Blocks: 100, Bavail: 25}

	var (
		mockController = gomock.NewController(t)
		mockExec       = ops.NewMockExecute(mockController)
		log            = logr.Logger{}
	)

	defer func() {
		mockController.Finish()
	}()

	type args struct {
		diskstat syscall.Statfs_t
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "check disk usage 25%% against 50%% threshold",
			args: args{
				diskstat: stat25Pct,
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "check disk usage 50%% against 50%% threshold",
			args: args{
				diskstat: stat50Pct,
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "check disk usage 75%% against 50%% threshold",
			args: args{
				diskstat: stat75Pct,
			},
			want:    true,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imageMgmtClient := NewImageMgmtClient(&log, mockExec, common.PathOutsideChroot(common.ContainerStoragePath))
			syscallStatfs = func(_ string, buf *syscall.Statfs_t) (err error) {
				*buf = tt.args.diskstat
				return nil
			}
			got, err := imageMgmtClient.CheckDiskUsageAgainstThreshold(50)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckDiskUsageAgainstThreshold() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("CheckDiskUsageAgainstThreshold() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_GetInuseImages(t *testing.T) {

	var (
		mockController = gomock.NewController(t)
		mockExec       = ops.NewMockExecute(mockController)
		log            = logr.Logger{}
	)

	defer func() {
		mockController.Finish()
	}()

	t.Run("test GetInUseImages", func(t *testing.T) {
		imageMgmtClient := NewImageMgmtClient(&log, mockExec, common.PathOutsideChroot(common.ContainerStoragePath))

		mockExec.EXPECT().Execute("crictl", "ps", "-a", "-o", "json").Return(test_CrictlPsOutput, nil)
		mockExec.EXPECT().Execute("podman", "ps", "-a", "--format", "json", "--log-level", "error").Return(test_PodmanPsOutput, nil)

		inUse, err := imageMgmtClient.GetInuseImages()
		if err != nil {
			t.Errorf("GetInuseImages() error = %v", err)
			return
		}

		// Order doesn't matter
		if len(inUse) != len(test_InUseImages) {
			t.Errorf("Expected %d images in use, got %d", len(test_InUseImages), len(inUse))
			return
		}

		for _, img := range inUse {
			if !lo.Contains(test_InUseImages, img) {
				t.Errorf("Returned list contains unexpected image %s", img)
				return
			}
		}
	})
}

func Test_GetPinnedImages(t *testing.T) {

	var (
		mockController = gomock.NewController(t)
		mockExec       = ops.NewMockExecute(mockController)
		log            = logr.Logger{}
	)

	defer func() {
		mockController.Finish()
	}()

	t.Run("test GetPinnedImages", func(t *testing.T) {
		imageMgmtClient := NewImageMgmtClient(&log, mockExec, common.PathOutsideChroot(common.ContainerStoragePath))

		mockExec.EXPECT().Execute("crictl", "images", "-o", "json").Return(test_CrictlImagesOutput, nil)

		pinned, err := imageMgmtClient.GetPinnedImages()
		if err != nil {
			t.Errorf("GetPinnedImages() error = %v", err)
			return
		}

		// Order doesn't matter
		if len(pinned) != len(test_PinnedImages) {
			t.Errorf("Expected %d pinned images, got %d", len(test_PinnedImages), len(pinned))
			return
		}

		for _, img := range pinned {
			if !lo.Contains(test_PinnedImages, img) {
				t.Errorf("Returned list contains unexpected image %s", img)
				return
			}
		}
	})
}

func Test_GetRemovalCandidates(t *testing.T) {

	var (
		mockController = gomock.NewController(t)
		mockExec       = ops.NewMockExecute(mockController)
		log            = logr.Logger{}
	)

	defer func() {
		mockController.Finish()
	}()

	t.Run("test GetRemovalCandidates", func(t *testing.T) {
		imageMgmtClient := NewImageMgmtClient(&log, mockExec, common.PathOutsideChroot(common.ContainerStoragePath))

		mockExec.EXPECT().Execute("crictl", "ps", "-a", "-o", "json").Return(test_CrictlPsOutput, nil)
		mockExec.EXPECT().Execute("podman", "ps", "-a", "--format", "json", "--log-level", "error").Return(test_PodmanPsOutput, nil)
		mockExec.EXPECT().Execute("crictl", "images", "-o", "json").Return(test_CrictlImagesOutput, nil)
		mockExec.EXPECT().Execute("podman", "images", "--format", "json", "--log-level", "error").Return(test_PodmanImagesOutput, nil)

		removalCandidates, err := imageMgmtClient.GetRemovalCandidates()
		if err != nil {
			t.Errorf("GetRemovalCandidates() error = %v", err)
			return
		} else if !reflect.DeepEqual(removalCandidates, test_RemovalCandidates) {
			t.Errorf("returned list: %v, expected = %v", removalCandidates, test_RemovalCandidates)
			return
		}
	})
}
