//
// Copyright 2020 IBM Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package k8smanifest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	k8smnfcosign "github.com/sigstore/k8s-manifest-sigstore/pkg/cosign"
	k8smnfutil "github.com/sigstore/k8s-manifest-sigstore/pkg/util"
	"github.com/sigstore/k8s-manifest-sigstore/pkg/util/kubeutil"
	mapnode "github.com/sigstore/k8s-manifest-sigstore/pkg/util/mapnode"
	sigtypes "github.com/sigstore/k8s-manifest-sigstore/pkg/util/sigtypes"
	pgp "github.com/sigstore/k8s-manifest-sigstore/pkg/util/sigtypes/pgp"
	x509 "github.com/sigstore/k8s-manifest-sigstore/pkg/util/sigtypes/x509"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

const SigRefEmbeddedInAnnotation = "__embedded_in_annotation__"

type SignatureVerifier interface {
	Verify() (bool, string, *int64, error)
}

func NewSignatureVerifier(objYAMLBytes []byte, sigRef string, pubkeyPath *string, annotationConfig AnnotationConfig) SignatureVerifier {
	var imageRef, resourceRef string
	if strings.HasPrefix(sigRef, InClusterObjectPrefix) {
		resourceRef = sigRef
	} else if sigRef != "" {
		imageRef = sigRef
	}

	imageRefAnnotationKey := annotationConfig.ImageRefAnnotationKey()
	annotations := k8smnfutil.GetAnnotationsInYAML(objYAMLBytes)
	if imageRef == "" {
		if annoImageRef, ok := annotations[imageRefAnnotationKey]; ok {
			imageRef = annoImageRef
		}
	}

	var pubkeyPathString *string
	if pubkeyPath != nil && *pubkeyPath != "" {
		pubkeyPathString = pubkeyPath
	}

	if imageRef != "" && imageRef != SigRefEmbeddedInAnnotation {
		return &ImageSignatureVerifier{imageRef: imageRef, onMemoryCacheEnabled: true, pubkeyPathString: pubkeyPathString, annotationConfig: annotationConfig}
	} else {
		return &BlobSignatureVerifier{annotations: annotations, resourceRef: resourceRef, pubkeyPathString: pubkeyPathString, annotationConfig: annotationConfig}
	}
}

type ImageSignatureVerifier struct {
	imageRef             string
	pubkeyPathString     *string
	onMemoryCacheEnabled bool
	annotationConfig     AnnotationConfig
}

func (v *ImageSignatureVerifier) Verify() (bool, string, *int64, error) {
	imageRef := v.imageRef
	if imageRef == "" {
		return false, "", nil, errors.New("no image reference is found")
	}

	pubkeyPathString := v.pubkeyPathString
	var pubkeys []string
	if pubkeyPathString != nil && *pubkeyPathString != "" {
		pubkeys = k8smnfutil.SplitCommaSeparatedString(*pubkeyPathString)
	} else {
		pubkeys = []string{""}
	}

	verified := false
	signerName := ""
	var signedTimestamp *int64
	var err error
	if v.onMemoryCacheEnabled {
		cacheFound := false
		cacheFoundCount := 0
		allErrs := []string{}
		for i := range pubkeys {
			pubkey := pubkeys[i]
			// try getting result from cache
			cacheFound, verified, signerName, signedTimestamp, err = v.getResultFromCache(imageRef, pubkey)
			// if found and verified true, return it
			if cacheFound {
				cacheFoundCount += 1
				if verified {
					return verified, signerName, signedTimestamp, err
				}
			}
			if err != nil {
				allErrs = append(allErrs, err.Error())
			}
		}
		if !verified && cacheFoundCount == len(pubkeys) {
			return false, "", nil, fmt.Errorf("signature verification failed: %s", strings.Join(allErrs, "; "))
		}
	}

	log.Debug("image signature cache not found")
	allErrs := []string{}
	for i := range pubkeys {
		pubkey := pubkeys[i]
		// do normal image verification
		verified, signerName, signedTimestamp, err = k8smnfcosign.VerifyImage(imageRef, pubkey)

		if v.onMemoryCacheEnabled {
			// set the result to cache
			v.setResultToCache(imageRef, pubkey, verified, signerName, signedTimestamp, err)
		}

		if verified {
			return verified, signerName, signedTimestamp, err
		} else if err != nil {
			allErrs = append(allErrs, err.Error())
		}
	}
	return false, "", nil, fmt.Errorf("signature verification failed: %s", strings.Join(allErrs, "; "))
}

func (v *ImageSignatureVerifier) getResultFromCache(imageRef, pubkey string) (bool, bool, string, *int64, error) {
	key := fmt.Sprintf("cache/verify-image/%s/%s", imageRef, pubkey)
	resultNum := 4
	result, err := k8smnfutil.GetCache(key)
	if err != nil {
		// OnMemoryCache.Get() returns an error only when the key was not found
		return false, false, "", nil, nil
	}
	if len(result) != resultNum {
		return false, false, "", nil, fmt.Errorf("cache returns inconsistent data: a length of verify image result must be %v, but got %v", resultNum, len(result))
	}
	verified := false
	signerName := ""
	var signedTimestamp *int64
	if result[0] != nil {
		verified = result[0].(bool)
	}
	if result[1] != nil {
		signerName = result[1].(string)
	}
	if result[2] != nil {
		signedTimestamp = result[2].(*int64)
	}
	if result[3] != nil {
		err = result[3].(error)
	}
	return true, verified, signerName, signedTimestamp, err
}

func (v *ImageSignatureVerifier) setResultToCache(imageRef, pubkey string, verified bool, signerName string, signedTimestamp *int64, err error) {
	key := fmt.Sprintf("cache/verify-image/%s/%s", imageRef, pubkey)
	setErr := k8smnfutil.SetCache(key, verified, signerName, signedTimestamp, err)
	if setErr != nil {
		log.Warn("cache set error: ", setErr.Error())
	}
}

type BlobSignatureVerifier struct {
	annotations      map[string]string
	resourceRef      string
	pubkeyPathString *string
	annotationConfig AnnotationConfig
}

func (v *BlobSignatureVerifier) Verify() (bool, string, *int64, error) {
	sigMap, err := v.getSignatures()
	if err != nil {
		return false, "", nil, errors.Wrap(err, "failed to get signature")
	}

	msgBytes := sigMap[MessageAnnotationBaseName]
	sigBytes := sigMap[SignatureAnnotationBaseName]
	certBytes := sigMap[CertificateAnnotationBaseName]
	bundleBytes := sigMap[BundleAnnotationBaseName]

	sigType := sigtypes.GetSignatureTypeFromPublicKey(v.pubkeyPathString)
	if sigType == sigtypes.SigTypeUnknown {
		return false, "", nil, errors.New("failed to judge signature type from public key configuration")
	} else if sigType == sigtypes.SigTypeCosign {
		return k8smnfcosign.VerifyBlob(msgBytes, sigBytes, certBytes, bundleBytes, v.pubkeyPathString)
	} else if sigType == sigtypes.SigTypePGP {
		return pgp.VerifyBlob(msgBytes, sigBytes, v.pubkeyPathString)
	} else if sigType == sigtypes.SigTypeX509 {
		return x509.VerifyBlob(msgBytes, sigBytes, certBytes, v.pubkeyPathString)
	}

	return false, "", nil, errors.New("unknown error")
}

func (v *BlobSignatureVerifier) getSignatures() (map[string][]byte, error) {
	sigMap := map[string][]byte{}
	var msg, sig, cert, bundle string
	var ok bool
	if v.resourceRef != "" {
		cmRef := v.resourceRef
		cm, err := GetConfigMapFromK8sObjectRef(cmRef)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get a configmap")
		}
		msg, ok = cm.Data[MessageAnnotationBaseName]
		if !ok {
			return nil, fmt.Errorf("`%s` is not found in the configmap %s", MessageAnnotationBaseName, cmRef)
		}
		sig, ok = cm.Data[SignatureAnnotationBaseName]
		if !ok {
			return nil, fmt.Errorf("`%s` is not found in the configmap %s", SignatureAnnotationBaseName, cmRef)
		}
		cert = cm.Data[CertificateAnnotationBaseName]
		bundle = cm.Data[BundleAnnotationBaseName]
	} else {
		annotations := v.annotations
		messageAnnotationKey := v.annotationConfig.MessageAnnotationKey()
		msg, ok = annotations[messageAnnotationKey]
		if !ok {
			return nil, fmt.Errorf("`%s` is not found in the annotations", messageAnnotationKey)
		}
		signatureAnnotationKey := v.annotationConfig.SignatureAnnotationKey()
		sig, ok = annotations[signatureAnnotationKey]
		if !ok {
			return nil, fmt.Errorf("`%s` is not found in the annotations", signatureAnnotationKey)
		}
		certificateAnnotationKey := v.annotationConfig.CertificateAnnotationKey()
		budnleAnnotationKey := v.annotationConfig.BundleAnnotationKey()
		cert = annotations[certificateAnnotationKey]
		bundle = annotations[budnleAnnotationKey]
	}
	if msg != "" {
		sigMap[MessageAnnotationBaseName] = []byte(msg)
	}
	if sig != "" {
		sigMap[SignatureAnnotationBaseName] = []byte(sig)
	}
	if cert != "" {
		sigMap[CertificateAnnotationBaseName] = []byte(cert)
	}
	if bundle != "" {
		sigMap[BundleAnnotationBaseName] = []byte(bundle)
	}
	return sigMap, nil
}

// This is an interface for fetching YAML manifest
// a function Fetch() fetches a YAML manifest which matches the input object's kind, name and so on
type ManifestFetcher interface {
	Fetch(objYAMLBytes []byte) ([][]byte, string, error)
}

// return a manifest fetcher.
// `imageRef` is used for judging if manifest is inside an image or not.
// `annotationConfig` is used for annotation domain config like "cosign.sigstore.dev".
// `ignoreFields` and `maxResourceManifestNum` are used inside manifest detection logic.
func NewManifestFetcher(imageRef, resourceRef string, annotationConfig AnnotationConfig, ignoreFields []string, maxResourceManifestNum int) ManifestFetcher {
	if imageRef != "" {
		return &ImageManifestFetcher{imageRefString: imageRef, AnnotationConfig: annotationConfig, ignoreFields: ignoreFields, maxResourceManifestNum: maxResourceManifestNum, cacheEnabled: true}
	} else {
		return &BlobManifestFetcher{AnnotationConfig: annotationConfig, resourceRefString: resourceRef, ignoreFields: ignoreFields, maxResourceManifestNum: maxResourceManifestNum}
	}
}

// ImageManifestFetcher is a fetcher implementation for image reference
type ImageManifestFetcher struct {
	imageRefString         string
	AnnotationConfig       AnnotationConfig
	ignoreFields           []string // used by ManifestSearchByValue()
	maxResourceManifestNum int      // used by ManifestSearchByValue()
	cacheEnabled           bool
}

func (f *ImageManifestFetcher) Fetch(objYAMLBytes []byte) ([][]byte, string, error) {
	imageRefString := f.imageRefString
	imageRefAnnotationKey := f.AnnotationConfig.ImageRefAnnotationKey()
	if imageRefString == "" {
		annotations := k8smnfutil.GetAnnotationsInYAML(objYAMLBytes)
		if annoImageRef, ok := annotations[imageRefAnnotationKey]; ok {
			imageRefString = annoImageRef
		}
	}
	if imageRefString == "" {
		return nil, "", errors.New("no image reference is found")
	}

	var maxResourceManifestNumPtr *int
	if f.maxResourceManifestNum > 0 {
		maxResourceManifestNumPtr = &f.maxResourceManifestNum
	}

	imageRefList := k8smnfutil.SplitCommaSeparatedString(imageRefString)
	for _, imageRef := range imageRefList {
		concatYAMLbytes, err := f.fetchManifestInSingleImage(imageRef)
		if err != nil {
			return nil, "", err
		}
		found, resourceManifests := k8smnfutil.FindManifestYAML(concatYAMLbytes, objYAMLBytes, maxResourceManifestNumPtr, f.ignoreFields)
		if found {
			return resourceManifests, imageRef, nil
		}
	}
	return nil, "", errors.New("failed to find a YAML manifest in the image")
}

func (f *ImageManifestFetcher) fetchManifestInSingleImage(singleImageRef string) ([]byte, error) {
	var concatYAMLbytes []byte
	var err error
	if f.cacheEnabled {
		cacheFound := false
		// try getting YAML manifests from cache
		cacheFound, concatYAMLbytes, err = f.getManifestFromCache(singleImageRef)
		// if cache not found, do fetch and set the result to cache
		if !cacheFound {
			log.Debug("image manifest cache not found")
			// fetch YAML manifests from actual image
			concatYAMLbytes, err = f.getConcatYAMLFromImageRef(singleImageRef)
			if err == nil {
				// set the result to cache
				f.setManifestToCache(singleImageRef, concatYAMLbytes, err)
			}
		}
	} else {
		// fetch YAML manifests from actual image
		concatYAMLbytes, err = f.getConcatYAMLFromImageRef(singleImageRef)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to get YAMLs in the image")
	}
	return concatYAMLbytes, nil
}

func (f *ImageManifestFetcher) FetchAll() ([][]byte, error) {
	imageRefString := f.imageRefString
	imageRefList := k8smnfutil.SplitCommaSeparatedString(imageRefString)

	yamls := [][]byte{}
	for _, imageRef := range imageRefList {
		concatYAMLbytes, err := f.fetchManifestInSingleImage(imageRef)
		if err != nil {
			return nil, err
		}
		yamlsInImage := k8smnfutil.SplitConcatYAMLs(concatYAMLbytes)
		yamls = append(yamls, yamlsInImage...)
	}
	return yamls, nil
}

func (f *ImageManifestFetcher) getConcatYAMLFromImageRef(imageRef string) ([]byte, error) {
	image, err := k8smnfutil.PullImage(imageRef)
	if err != nil {
		return nil, err
	}
	concatYAMLbytes, err := k8smnfutil.GenerateConcatYAMLsFromImage(image)
	if err != nil {
		return nil, err
	}
	return concatYAMLbytes, nil
}

func (f *ImageManifestFetcher) getManifestFromCache(imageRef string) (bool, []byte, error) {
	key := fmt.Sprintf("cache/fetch-manifest/%s", imageRef)
	resultNum := 2
	result, err := k8smnfutil.GetCache(key)
	if err != nil {
		// OnMemoryCache.Get() returns an error only when the key was not found
		return false, nil, nil
	}
	if len(result) != resultNum {
		return false, nil, fmt.Errorf("cache returns inconsistent data: a length of fetch manifest result must be %v, but got %v", resultNum, len(result))
	}
	var concatYAMLbytes []byte
	if result[0] != nil {
		var ok bool
		if concatYAMLbytes, ok = result[0].([]byte); !ok {
			concatYAMLStr := result[0].(string)
			if tmpYAMLbytes, err := base64.StdEncoding.DecodeString(concatYAMLStr); err == nil {
				concatYAMLbytes = tmpYAMLbytes
			}
		}
	}
	if result[1] != nil {
		err = result[1].(error)
	}
	return true, concatYAMLbytes, err
}

func (f *ImageManifestFetcher) setManifestToCache(imageRef string, concatYAMLbytes []byte, err error) {
	key := fmt.Sprintf("cache/fetch-manifest/%s", imageRef)
	setErr := k8smnfutil.SetCache(key, concatYAMLbytes, err)
	if setErr != nil {
		log.Warn("cache set error: ", setErr.Error())
	}
}

type BlobManifestFetcher struct {
	AnnotationConfig       AnnotationConfig
	resourceRefString      string
	ignoreFields           []string // used by ManifestSearchByValue()
	maxResourceManifestNum int      // used by ManifestSearchByValue()
}

func (f *BlobManifestFetcher) Fetch(objYAMLBytes []byte) ([][]byte, string, error) {
	if f.resourceRefString != "" {
		return f.fetchManifestFromResource(objYAMLBytes)
	}

	annotations := k8smnfutil.GetAnnotationsInYAML(objYAMLBytes)

	messageAnnotationKey := f.AnnotationConfig.MessageAnnotationKey()
	base64Msg, messageFound := annotations[messageAnnotationKey]
	if !messageFound {
		return nil, "", nil
	}
	gzipMsg, err := base64.StdEncoding.DecodeString(base64Msg)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to decode base64 message in the annotation")
	}
	// `gzipMsg` is a gzip compressed .tar.gz file, so get a tar ball by decompressing it
	gzipTarBall := k8smnfutil.GzipDecompress(gzipMsg)

	yamls, err := k8smnfutil.GetYAMLsInArtifact(gzipTarBall)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to read YAMLs in the gzipped message")
	}

	concatYAMLbytes := k8smnfutil.ConcatenateYAMLs(yamls)

	var maxResourceManifestNumPtr *int
	if f.maxResourceManifestNum > 0 {
		maxResourceManifestNumPtr = &f.maxResourceManifestNum
	}

	found, resourceManifests := k8smnfutil.FindManifestYAML(concatYAMLbytes, objYAMLBytes, maxResourceManifestNumPtr, f.ignoreFields)
	if !found {
		return nil, "", errors.New("failed to find a YAML manifest in the gzipped message")
	}
	return resourceManifests, SigRefEmbeddedInAnnotation, nil
}

func (f *BlobManifestFetcher) fetchManifestFromResource(objYAMLBytes []byte) ([][]byte, string, error) {
	resourceRefString := f.resourceRefString
	if resourceRefString == "" {
		return nil, "", errors.New("no signature resource reference is specified")
	}

	var maxResourceManifestNumPtr *int
	if f.maxResourceManifestNum > 0 {
		maxResourceManifestNumPtr = &f.maxResourceManifestNum
	}

	resourceRefList := k8smnfutil.SplitCommaSeparatedString(resourceRefString)
	for _, resourceRef := range resourceRefList {
		concatYAMLbytes, err := f.fetchManifestInSingleConfigMap(resourceRef)
		if err != nil {
			return nil, "", err
		}
		found, resourceManifests := k8smnfutil.FindManifestYAML(concatYAMLbytes, objYAMLBytes, maxResourceManifestNumPtr, f.ignoreFields)
		if found {
			return resourceManifests, resourceRef, nil
		}
	}
	return nil, "", errors.New("failed to find a YAML manifest in the specified signature configmaps")
}

func (f *BlobManifestFetcher) fetchManifestInSingleConfigMap(singleCMRef string) ([]byte, error) {
	cm, err := GetConfigMapFromK8sObjectRef(singleCMRef)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get a configmap")
	}
	base64Msg, messageFound := cm.Data[MessageAnnotationBaseName]
	if !messageFound {
		return nil, fmt.Errorf("failed to find `%s` in a configmap %s", MessageAnnotationBaseName, cm.GetName())
	}
	gzipMsg, err := base64.StdEncoding.DecodeString(base64Msg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode base64 message in the configmap")
	}
	// `gzipMsg` is a gzip compressed .tar.gz file, so get a tar ball by decompressing it
	gzipTarBall := k8smnfutil.GzipDecompress(gzipMsg)

	yamls, err := k8smnfutil.GetYAMLsInArtifact(gzipTarBall)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read YAMLs in the gzipped message")
	}
	concatYAMLbytes := k8smnfutil.ConcatenateYAMLs(yamls)
	return concatYAMLbytes, nil
}

type VerifyResult struct {
	Verified bool                `json:"verified"`
	Signer   string              `json:"signer"`
	Diff     *mapnode.DiffResult `json:"diff"`
}

func (r *VerifyResult) String() string {
	rB, _ := json.Marshal(r)
	return string(rB)
}

func GetConfigMapFromK8sObjectRef(objRef string) (*corev1.ConfigMap, error) {
	kind, ns, name, err := parseObjectInCluster(objRef)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse a configmap reference")
	}
	if kind != "ConfigMap" && kind != "configmaps" && kind != "cm" {
		return nil, fmt.Errorf("configmap reference must be \"k8s://ConfigMap/[NAMESPACE]/[NAME]\", but got %s", objRef)
	}
	cmObj, err := kubeutil.GetResource("", kind, ns, name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get a configmap")
	}
	cmBytes, err := json.Marshal(cmObj.Object)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal a configmap")
	}
	var cm *corev1.ConfigMap
	err = json.Unmarshal(cmBytes, &cm)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal a configmap bytes")
	}
	return cm, nil
}
