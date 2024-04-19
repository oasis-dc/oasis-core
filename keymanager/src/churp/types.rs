//! CHURP types used by the worker-host protocol.
use oasis_core_runtime::{
    common::{
        crypto::{
            hash::Hash,
            signature::{PublicKey, Signature},
        },
        namespace::Namespace,
    },
    consensus::beacon::EpochTime,
};

/// Handoff request.
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct HandoffRequest {
    /// A unique identifier within the key manager runtime.
    pub id: u8,

    /// The identifier of the key manager runtime.
    pub runtime_id: Namespace,

    /// The epoch of the handoff.
    pub epoch: EpochTime,
}

/// Handoff query.
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct QueryRequest {
    /// A unique identifier within the key manager runtime.
    pub id: u8,

    /// The identifier of the key manager runtime.
    pub runtime_id: Namespace,

    /// The epoch of the handoff.
    pub epoch: EpochTime,

    /// The public key of the node making the query.
    #[cbor(optional)]
    pub node_id: Option<PublicKey>,
}

/// Fetch handoff data request.
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct FetchRequest {
    /// A unique identifier within the key manager runtime.
    pub id: u8,

    /// The identifier of the key manager runtime.
    pub runtime_id: Namespace,

    /// The epoch of the handoff.
    pub epoch: EpochTime,

    /// The public keys of nodes from which to fetch data.
    pub node_ids: Vec<PublicKey>,
}

/// Fetch handoff data response.
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct FetchResponse {
    /// Indicates whether the data fetching was completed.
    pub completed: bool,

    /// Public keys of nodes from which data was successfully fetched.
    pub succeeded: Vec<PublicKey>,

    /// Public keys of nodes from which data failed to be fetched.
    pub failed: Vec<PublicKey>,
}

/// Node's application to form a new committee.
#[derive(Clone, Debug, Default, PartialEq, Eq, cbor::Encode, cbor::Decode)]
pub struct ApplicationRequest {
    /// A unique identifier within the key manager runtime.
    pub id: u8,

    /// The identifier of the key manager runtime.
    pub runtime_id: Namespace,

    /// The epoch of the handoff for which the node would like to register.
    pub epoch: EpochTime,

    /// Checksum is the hash of the verification matrix.
    pub checksum: Hash,
}

/// An application request signed by the key manager enclave using its
/// runtime attestation key (RAK).
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct SignedApplicationRequest {
    /// Application request.
    pub application: ApplicationRequest,

    /// RAK signature of the application request.
    pub signature: Signature,
}

/// Confirmation that the node successfully reconstructed the share.
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct ConfirmationRequest {
    /// A unique identifier within the key manager runtime.
    pub id: u8,

    /// The identifier of the key manager runtime.
    pub runtime_id: Namespace,

    /// The epoch of the handoff for which the node reconstructed the share.
    pub epoch: EpochTime,

    /// Checksum is the hash of the verification matrix.
    pub checksum: Hash,
}

/// A confirmation request signed by the key manager enclave using its
/// runtime attestation key (RAK).
#[derive(Clone, Debug, Default, cbor::Encode, cbor::Decode)]
pub struct SignedConfirmationRequest {
    /// Confirmation request.
    pub confirmation: ConfirmationRequest,

    /// RAK signature of the confirmation request.
    pub signature: Signature,
}

/// Encoded secret share.
#[derive(Clone, Default, cbor::Encode, cbor::Decode)]
pub struct EncodedSecretShare {
    /// Encoded polynomial.
    pub polynomial: Vec<u8>,

    /// Encoded verification matrix.
    pub verification_matrix: Vec<u8>,
}
